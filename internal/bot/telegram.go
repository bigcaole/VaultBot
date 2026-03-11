package bot

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"vaultbot/internal/config"
	"vaultbot/internal/crypto"
	"vaultbot/internal/model"
	"vaultbot/internal/store"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gorm.io/gorm"
)

// TelegramBot handles Telegram updates.
type TelegramBot struct {
	bot        *tgbotapi.BotAPI
	cfg        *config.Config
	db         *gorm.DB
	store      *store.RedisStore
	derivedKey []byte
	legacyKey  []byte
	ctx        context.Context
	menuMu     sync.Mutex
	menuMsg    map[string]int
	cmdMu      sync.Mutex
	cmdScope   map[int64]string
}

const (
	searchTTL        = 10 * time.Minute
	editTTL          = 10 * time.Minute
	menuTTL          = 10 * time.Minute
	passwordQueryTTL = 180 * time.Second
)

// StartTelegramBot initializes the bot and starts the update loop.
func StartTelegramBot(ctx context.Context, cfg *config.Config, db *gorm.DB, store *store.RedisStore, derivedKey []byte, legacyKey []byte) (*TelegramBot, error) {
	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required to start Telegram bot")
	}
	if strings.TrimSpace(cfg.UnlockPIN) == "" {
		return nil, fmt.Errorf("UNLOCK_PIN is required to start Telegram bot")
	}
	bot, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, err
	}
	commands := []tgbotapi.BotCommand{
		{Command: "menu", Description: "打开主菜单"},
		{Command: "start", Description: "显示功能入口"},
		{Command: "unlock", Description: "解锁密码查询"},
		{Command: "add", Description: "新增账号"},
		{Command: "find", Description: "平台关键词查询"},
		{Command: "search", Description: "按字段搜索"},
		{Command: "list", Description: "分类浏览"},
		{Command: "ttl", Description: "设置自动删除"},
		{Command: "cancel", Description: "取消当前流程"},
		{Command: "help", Description: "帮助说明"},
	}
	if _, err := bot.Request(tgbotapi.NewSetMyCommands(commands...)); err != nil {
		log.Printf("telegram set commands failed: %v", err)
	}
	b := &TelegramBot{
		bot:        bot,
		cfg:        cfg,
		db:         db,
		store:      store,
		derivedKey: derivedKey,
		legacyKey:  legacyKey,
		ctx:        ctx,
		menuMsg:    make(map[string]int),
		cmdScope:   make(map[int64]string),
	}
	if _, err := StartBackupScheduler(ctx, bot, cfg, db); err != nil {
		return nil, err
	}
	go b.run()
	return b, nil
}

func (b *TelegramBot) run() {
	update := tgbotapi.NewUpdate(0)
	update.Timeout = 30
	updates := b.bot.GetUpdatesChan(update)
	for {
		select {
		case <-b.ctx.Done():
			return
		case u := <-updates:
			if u.Message != nil {
				b.handleMessage(u.Message)
			}
			if u.CallbackQuery != nil {
				b.handleCallback(u.CallbackQuery)
			}
		}
	}
}

func (b *TelegramBot) handleMessage(msg *tgbotapi.Message) {
	if msg.From == nil {
		return
	}
	userID := fmt.Sprintf("%d", msg.From.ID)
	isOperator := IsAllowed(b.cfg.AllowedUserIDs, userID)
	isBackupReceiver := IsAllowed(b.cfg.BackupReceiverIDs, userID)
	if msg.Chat != nil && msg.Chat.Type != "private" && !b.cfg.AllowGroupChat {
		if isOperator {
			b.reply(msg.Chat.ID, "请在私聊中使用该机器人。")
		} else if !isBackupReceiver {
			b.audit(userID, "unauthorized", "")
		}
		return
	}
	if isBackupReceiver && !isOperator {
		b.ensureChatCommands(msg.Chat.ID, "backup")
		if msg.IsCommand() {
			switch msg.Command() {
			case "menu", "start":
				b.sendBackupMenu(msg.Chat.ID, userID, 0)
			case "help":
				b.sendBackupHelp(msg.Chat.ID, userID, 0)
			case "ping":
				b.reply(msg.Chat.ID, "连接正常。")
			case "backup":
				b.runManualBackup(msg.Chat.ID, userID, false)
			case "backup_test":
				b.runBackupTest(msg.Chat.ID, userID)
			default:
				b.sendBackupHelp(msg.Chat.ID, userID, 0)
			}
		}
		return
	}
	if !isOperator {
		if !isBackupReceiver {
			b.audit(userID, "unauthorized", "")
		}
		return
	}
	b.ensureChatCommands(msg.Chat.ID, "operator")
	if msg.MessageID != 0 {
		b.deleteMessageLaterForUser(msg.Chat.ID, msg.MessageID, userID)
	}
	if b.store != nil {
		allowed, err := b.store.Allow(context.Background(), "rate:tg:"+userID, 20, time.Minute)
		if err != nil {
			log.Printf("telegram rate limit error user_id=%s err=%v", userID, err)
		} else if !allowed {
			b.reply(msg.Chat.ID, "请求过于频繁，请稍后重试。")
			return
		}
	}

	if msg.IsCommand() {
		b.clearUserStates(userID, msg.Command())
		switch msg.Command() {
		case "menu":
			b.sendMainMenu(msg.Chat.ID, userID, 0)
		case "start":
			b.sendMainMenu(msg.Chat.ID, userID, 0)
		case "unlock":
			b.handleUnlock(msg.Chat.ID, userID, msg.CommandArguments())
		case "add":
			b.startAddFlow(msg.Chat.ID, userID)
		case "help":
			b.sendHelpMenu(msg.Chat.ID, userID, 0)
		case "find":
			if !b.requireUnlockedForQuery(msg.Chat.ID, userID, 0) {
				return
			}
			query := strings.TrimSpace(msg.CommandArguments())
			if query == "" {
				b.audit(userID, "find", "")
				b.sendCategoryMenu(msg.Chat.ID, userID, 0)
				return
			}
			b.handleFind(msg.Chat.ID, userID, query)
		case "search":
			if !b.requireUnlockedForQuery(msg.Chat.ID, userID, 0) {
				return
			}
			b.sendSearchFieldMenu(msg.Chat.ID, userID, 0)
		case "ttl":
			b.sendTTLMenu(msg.Chat.ID, userID, 0)
		case "backup":
			b.runManualBackup(msg.Chat.ID, userID, true)
		case "list":
			if !b.requireUnlockedForQuery(msg.Chat.ID, userID, 0) {
				return
			}
			b.sendCategoryMenu(msg.Chat.ID, userID, 0)
		case "cancel":
			_ = b.store.Del(context.Background(), stateKey("tg:add", userID))
			b.reply(msg.Chat.ID, "已取消当前操作。")
		default:
			b.sendHelpMenu(msg.Chat.ID, userID, 0)
		}
		return
	}

	if b.handleEditInput(msg.Chat.ID, userID, strings.TrimSpace(msg.Text)) {
		return
	}
	if b.handleCategoryEditInput(msg.Chat.ID, userID, strings.TrimSpace(msg.Text)) {
		return
	}
	if b.handleSearchInput(msg.Chat.ID, userID, strings.TrimSpace(msg.Text)) {
		return
	}
	b.handleAddStep(msg.Chat.ID, userID, strings.TrimSpace(msg.Text))
}

func (b *TelegramBot) handleUnlock(chatID int64, userID string, args string) {
	pin := strings.TrimSpace(args)
	if pin == "" {
		b.reply(chatID, "用法：/unlock <PIN>")
		return
	}
	if isSixDigitPIN(b.cfg.UnlockPIN) && !isSixDigitPIN(pin) {
		b.reply(chatID, "PIN 必须为 6 位数字。")
		return
	}
	if b.store == nil {
		b.reply(chatID, "会话存储不可用，请检查 Redis 配置。")
		return
	}
	if subtle.ConstantTimeCompare([]byte(pin), []byte(b.cfg.UnlockPIN)) != 1 {
		b.reply(chatID, "PIN 错误。")
		return
	}
	if err := b.store.Set(context.Background(), unlockKey(userID), "1", b.cfg.UnlockTTL); err != nil {
		b.reply(chatID, "解锁失败，请稍后重试。")
		return
	}
	b.reply(chatID, fmt.Sprintf("已解锁，%d 分钟内有效。", int(b.cfg.UnlockTTL.Minutes())))
}

func (b *TelegramBot) requireUnlocked(chatID int64, userID string) bool {
	if b.store == nil {
		b.reply(chatID, "会话存储不可用，请检查 Redis 配置。")
		return false
	}
	val, err := b.store.Get(context.Background(), unlockKey(userID))
	if err != nil || val == "" {
		b.reply(chatID, b.unlockHint())
		return false
	}
	return true
}

func (b *TelegramBot) requireUnlockedForQuery(chatID int64, userID string, messageID int) bool {
	if b.store == nil {
		b.updateMenu(chatID, userID, messageID, "会话存储不可用，请检查 Redis 配置。", backMainKeyboard())
		return false
	}
	val, err := b.store.Get(context.Background(), unlockKey(userID))
	if err != nil || val == "" {
		b.updateMenu(chatID, userID, messageID, b.unlockHint(), backMainKeyboard())
		return false
	}
	return true
}

func unlockKey(userID string) string {
	return "tg:unlock:" + userID
}

func (b *TelegramBot) unlockHint() string {
	base := fmt.Sprintf("请先使用 /unlock <PIN> 解锁，%d 分钟内有效。", int(b.cfg.UnlockTTL.Minutes()))
	if isSixDigitPIN(b.cfg.UnlockPIN) {
		return base + "（PIN 为 6 位数字）"
	}
	return base
}

func isSixDigitPIN(pin string) bool {
	if len(pin) != 6 {
		return false
	}
	for _, ch := range pin {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func (b *TelegramBot) startAddFlow(chatID int64, userID string) {
	if b.store == nil {
		b.reply(chatID, "会话存储不可用，请检查 Redis 配置。")
		return
	}
	st := &addState{Step: stepPlatform}
	if err := saveState(context.Background(), b.store, stateKey("tg:add", userID), st, 15*time.Minute); err != nil {
		b.reply(chatID, "系统繁忙，请稍后重试。")
		return
	}
	b.reply(chatID, "请输入平台名称：")
}

func (b *TelegramBot) handleAddStep(chatID int64, userID string, text string) {
	key := stateKey("tg:add", userID)
	if b.store == nil {
		b.reply(chatID, "会话存储不可用，请检查 Redis 配置。")
		return
	}
	st, err := loadState(context.Background(), b.store, key)
	if err != nil {
		b.reply(chatID, "系统繁忙，请稍后重试。")
		return
	}
	if st == nil {
		return
	}

	switch st.Step {
	case stepPlatform:
		st.Platform = text
		st.Step = stepCategory
		if err := saveState(context.Background(), b.store, key, st, 15*time.Minute); err != nil {
			b.reply(chatID, "系统繁忙，请稍后重试。")
			return
		}
		b.reply(chatID, "请输入分类（如：工作/生活/金融）：")
	case stepCategory:
		st.Category = text
		st.Step = stepUsername
		if err := saveState(context.Background(), b.store, key, st, 15*time.Minute); err != nil {
			b.reply(chatID, "系统繁忙，请稍后重试。")
			return
		}
		b.reply(chatID, "请输入用户名：")
	case stepUsername:
		st.Username = text
		st.Step = stepPassword
		if err := saveState(context.Background(), b.store, key, st, 15*time.Minute); err != nil {
			b.reply(chatID, "系统繁忙，请稍后重试。")
			return
		}
		b.reply(chatID, "请输入密码（不会写入日志）：")
	case stepPassword:
		ciphertext, nonce, err := crypto.Encrypt(text, b.derivedKey)
		if err != nil {
			b.reply(chatID, "加密失败，请稍后重试。")
			return
		}
		st.EncryptedPassword = ciphertext
		st.Nonce = nonce
		st.Step = stepEmail
		if err := saveState(context.Background(), b.store, key, st, 15*time.Minute); err != nil {
			b.reply(chatID, "系统繁忙，请稍后重试。")
			return
		}
		b.reply(chatID, "请输入邮箱（可输入 - 跳过）：")
	case stepEmail:
		if text == "-" {
			text = ""
		}
		st.Email = text
		st.Step = stepPhone
		if err := saveState(context.Background(), b.store, key, st, 15*time.Minute); err != nil {
			b.reply(chatID, "系统繁忙，请稍后重试。")
			return
		}
		b.reply(chatID, "请输入手机号（可输入 - 跳过）：")
	case stepPhone:
		if text == "-" {
			text = ""
		}
		st.Phone = text
		st.Step = stepNotes
		if err := saveState(context.Background(), b.store, key, st, 15*time.Minute); err != nil {
			b.reply(chatID, "系统繁忙，请稍后重试。")
			return
		}
		b.reply(chatID, "请输入备注（可输入 - 跳过）：")
	case stepNotes:
		if text == "-" {
			text = ""
		}
		st.Notes = text
		b.finishAddFlow(chatID, userID, st)
	}
}

func (b *TelegramBot) finishAddFlow(chatID int64, userID string, st *addState) {
	account := &model.Account{
		Platform:          st.Platform,
		Category:          st.Category,
		Username:          st.Username,
		EncryptedPassword: st.EncryptedPassword,
		Email:             st.Email,
		Phone:             st.Phone,
		Notes:             st.Notes,
		Nonce:             st.Nonce,
	}
	if err := b.db.Create(account).Error; err != nil {
		b.reply(chatID, "保存失败，请稍后重试。")
		return
	}
	b.audit(userID, "add", account.Platform)
	_ = b.store.Del(context.Background(), stateKey("tg:add", userID))
	b.reply(chatID, "已保存。")
}

func (b *TelegramBot) handleFind(chatID int64, userID string, query string) {
	if !b.requireUnlocked(chatID, userID) {
		return
	}
	if query == "" {
		b.updateMenu(chatID, userID, 0, "请输入平台关键词，例如：/find github", backMainKeyboard())
		return
	}
	var accounts []model.Account
	if err := b.db.Where("platform ILIKE ?", "%"+query+"%").Order("platform").Find(&accounts).Error; err != nil {
		b.updateMenu(chatID, userID, 0, "查询失败。", backMainKeyboard())
		return
	}
	b.audit(userID, "find", query)
	if len(accounts) == 0 {
		b.updateMenu(chatID, userID, 0, "未找到记录。", backMainKeyboard())
		return
	}
	b.sendAccountsMenu(chatID, userID, 0, "搜索结果：", accounts)
}

func (b *TelegramBot) reply(chatID int64, text string) {
	if _, err := b.bot.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		log.Printf("telegram send failed chat_id=%d err=%v", chatID, err)
	}
}

func (b *TelegramBot) deleteMessageLater(chatID int64, messageID int) {
	go func() {
		timer := time.NewTimer(b.cfg.DeleteAfter)
		defer timer.Stop()
		select {
		case <-b.ctx.Done():
			return
		case <-timer.C:
			b.deleteMessageWithRetry(chatID, messageID)
		}
	}()
}

func (b *TelegramBot) deleteMessageAfter(chatID int64, messageID int, delay time.Duration) {
	if delay <= 0 {
		return
	}
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-b.ctx.Done():
			return
		case <-timer.C:
			b.deleteMessageWithRetry(chatID, messageID)
		}
	}()
}

func (b *TelegramBot) deleteMessageWithRetry(chatID int64, messageID int) {
	const maxAttempts = 3
	for i := 0; i < maxAttempts; i++ {
		_, err := b.bot.Request(tgbotapi.NewDeleteMessage(chatID, messageID))
		if err == nil {
			return
		}
		log.Printf("telegram delete failed chat_id=%d message_id=%d attempt=%d err=%v", chatID, messageID, i+1, err)
		time.Sleep(time.Duration(i+1) * time.Second)
	}
}

func (b *TelegramBot) deleteMessageLaterForUser(chatID int64, messageID int, userID string) {
	delay := b.userDeleteAfter(userID)
	if delay <= 0 {
		return
	}
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-b.ctx.Done():
			return
		case <-timer.C:
			b.deleteMessageWithRetry(chatID, messageID)
		}
	}()
}

func (b *TelegramBot) userDeleteAfter(userID string) time.Duration {
	if b.store == nil {
		return b.cfg.DeleteAfter
	}
	val, err := b.store.Get(context.Background(), "tg:ttl:"+userID)
	if err != nil || val == "" {
		return b.cfg.DeleteAfter
	}
	seconds, err := strconv.Atoi(val)
	if err != nil || seconds <= 0 {
		return b.cfg.DeleteAfter
	}
	return time.Duration(seconds) * time.Second
}

func (b *TelegramBot) clearUserStates(userID string, command string) {
	if b.store == nil {
		return
	}
	if command != "add" {
		_ = b.store.Del(context.Background(), stateKey("tg:add", userID))
	}
	if command != "search" {
		_ = b.store.Del(context.Background(), stateKey("tg:search", userID))
	}
	if command != "edit" {
		_ = b.store.Del(context.Background(), stateKey("tg:edit", userID))
	}
	if command != "cat_edit" {
		_ = b.store.Del(context.Background(), stateKey("tg:cat_edit", userID))
	}
}

func (b *TelegramBot) sendMainMenu(chatID int64, userID string, messageID int) {
	text := "VaultBot 主菜单："
	buttons := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("新增账号", "menu:add"),
		tgbotapi.NewInlineKeyboardButtonData("分类浏览", "menu:find"),
		tgbotapi.NewInlineKeyboardButtonData("字段搜索", "menu:search"),
		tgbotapi.NewInlineKeyboardButtonData("自动删除", "menu:ttl"),
		tgbotapi.NewInlineKeyboardButtonData("手动备份", "menu:backup"),
		tgbotapi.NewInlineKeyboardButtonData("帮助", "menu:help"),
	}
	keyboard := buildInlineKeyboard(buttons, 2)
	b.updateMenu(chatID, userID, messageID, text, keyboard)
}

func (b *TelegramBot) sendBackupMenu(chatID int64, userID string, messageID int) {
	text := "备份接收人菜单："
	b.updateMenuNoDelete(chatID, userID, messageID, text, backupMenuKeyboard())
}

func backupMenuKeyboard() tgbotapi.InlineKeyboardMarkup {
	buttons := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("连接测试", "backup:ping"),
		tgbotapi.NewInlineKeyboardButtonData("手动备份", "backup:run"),
		tgbotapi.NewInlineKeyboardButtonData("备份测试", "backup:test"),
		tgbotapi.NewInlineKeyboardButtonData("帮助", "backup:help"),
	}
	return buildInlineKeyboard(buttons, 2)
}

func (b *TelegramBot) sendCategoryMenu(chatID int64, userID string, messageID int) {
	var categories []string
	if err := b.db.Model(&model.Account{}).Distinct("category").Order("category").Pluck("category", &categories).Error; err != nil {
		b.reply(chatID, "查询失败。")
		return
	}
	if len(categories) == 0 {
		b.reply(chatID, "暂无记录。")
		return
	}
	if b.store != nil {
		_ = saveCategoryState(context.Background(), b.store, stateKey("tg:catmap", userID), &categoryState{Categories: categories}, menuTTL)
	}
	buttons := make([]tgbotapi.InlineKeyboardButton, 0, len(categories))
	for i, cat := range categories {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(cat, fmt.Sprintf("cat:%d", i)))
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("✏️修改", fmt.Sprintf("cat_edit:%d", i)))
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("🗑删除", fmt.Sprintf("cat_del:%d", i)))
	}
	buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("➕新增分类", "cat_add"))
	buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("返回主菜单", "back:main"))
	keyboard := buildInlineKeyboard(buttons, 3)
	b.updateMenu(chatID, userID, messageID, "请选择分类：", keyboard)
}

func (b *TelegramBot) sendAccountsMenu(chatID int64, userID string, messageID int, title string, accounts []model.Account) {
	buttons := make([]tgbotapi.InlineKeyboardButton, 0, len(accounts))
	for _, acc := range accounts {
		label := fmt.Sprintf("%s (%s)", acc.Platform, acc.Username)
		data := "acct:" + acc.ID.String()
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(label, data))
	}
	buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("返回分类", "back:categories"))
	keyboard := buildInlineKeyboard(buttons, 1)
	b.updateMenu(chatID, userID, messageID, title, keyboard)
}

func (b *TelegramBot) sendSearchFieldMenu(chatID int64, userID string, messageID int) {
	buttons := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("平台", "searchfield:platform"),
		tgbotapi.NewInlineKeyboardButtonData("分类", "searchfield:category"),
		tgbotapi.NewInlineKeyboardButtonData("用户名", "searchfield:username"),
		tgbotapi.NewInlineKeyboardButtonData("邮箱", "searchfield:email"),
		tgbotapi.NewInlineKeyboardButtonData("手机号", "searchfield:phone"),
		tgbotapi.NewInlineKeyboardButtonData("备注", "searchfield:notes"),
		tgbotapi.NewInlineKeyboardButtonData("全部字段", "searchfield:all"),
		tgbotapi.NewInlineKeyboardButtonData("返回主菜单", "back:main"),
	}
	keyboard := buildInlineKeyboard(buttons, 2)
	b.updateMenu(chatID, userID, messageID, "请选择搜索字段：", keyboard)
}

func (b *TelegramBot) handleSearchInput(chatID int64, userID string, text string) bool {
	if b.store == nil {
		return false
	}
	if !b.requireUnlocked(chatID, userID) {
		return true
	}
	st, err := loadSearchState(context.Background(), b.store, stateKey("tg:search", userID))
	if err != nil || st == nil {
		return false
	}
	_ = b.store.Del(context.Background(), stateKey("tg:search", userID))
	if text == "" {
		b.updateMenu(chatID, userID, 0, "请输入关键词。", backMainKeyboard())
		return true
	}
	var accounts []model.Account
	query := b.db.Model(&model.Account{})
	like := "%" + text + "%"
	switch st.Field {
	case "platform":
		query = query.Where("platform ILIKE ?", like)
	case "category":
		query = query.Where("category ILIKE ?", like)
	case "username":
		query = query.Where("username ILIKE ?", like)
	case "email":
		query = query.Where("email ILIKE ?", like)
	case "phone":
		query = query.Where("phone ILIKE ?", like)
	case "notes":
		query = query.Where("notes ILIKE ?", like)
	default:
		query = query.Where("platform ILIKE ? OR category ILIKE ? OR username ILIKE ? OR email ILIKE ? OR phone ILIKE ? OR notes ILIKE ?",
			like, like, like, like, like, like)
	}
	if err := query.Order("platform").Find(&accounts).Error; err != nil {
		b.updateMenu(chatID, userID, 0, "查询失败。", backMainKeyboard())
		return true
	}
	if len(accounts) == 0 {
		b.updateMenu(chatID, userID, 0, "未找到记录。", backMainKeyboard())
		return true
	}
	b.sendAccountsMenu(chatID, userID, 0, "搜索结果：", accounts)
	return true
}

func (b *TelegramBot) handleEditInput(chatID int64, userID string, text string) bool {
	if b.store == nil {
		return false
	}
	st, err := loadEditState(context.Background(), b.store, stateKey("tg:edit", userID))
	if err != nil || st == nil {
		return false
	}
	_ = b.store.Del(context.Background(), stateKey("tg:edit", userID))
	if text == "" {
		b.updateMenu(chatID, userID, 0, "请输入内容。", backMainKeyboard())
		return true
	}
	var account model.Account
	if err := b.db.First(&account, "id = ?", st.AccountID).Error; err != nil {
		b.updateMenu(chatID, userID, 0, "记录不存在。", backMainKeyboard())
		return true
	}
	switch st.Field {
	case "platform":
		account.Platform = text
	case "category":
		account.Category = text
	case "username":
		account.Username = text
	case "password":
		ciphertext, nonce, err := crypto.Encrypt(text, b.derivedKey)
		if err != nil {
			b.reply(chatID, "加密失败，请稍后重试。")
			return true
		}
		account.EncryptedPassword = ciphertext
		account.Nonce = nonce
	case "email":
		account.Email = text
	case "phone":
		account.Phone = text
	case "notes":
		account.Notes = text
	default:
		b.updateMenu(chatID, userID, 0, "不支持的字段。", backMainKeyboard())
		return true
	}
	if err := b.db.Save(&account).Error; err != nil {
		b.updateMenu(chatID, userID, 0, "更新失败。", backMainKeyboard())
		return true
	}
	b.updateMenu(chatID, userID, 0, "已更新。", backMainKeyboard())
	return true
}

func (b *TelegramBot) handleCategoryEditInput(chatID int64, userID string, text string) bool {
	if b.store == nil {
		return false
	}
	st, err := loadCategoryEditState(context.Background(), b.store, stateKey("tg:cat_edit", userID))
	if err != nil || st == nil {
		return false
	}
	_ = b.store.Del(context.Background(), stateKey("tg:cat_edit", userID))
	if text == "" {
		b.updateMenu(chatID, userID, 0, "请输入新的分类名称。", backMainKeyboard())
		return true
	}
	if st.Mode == "add" {
		stAdd := &addState{
			Step:     stepUsername,
			Category: text,
		}
		if err := saveState(context.Background(), b.store, stateKey("tg:add", userID), stAdd, 15*time.Minute); err != nil {
			b.updateMenu(chatID, userID, 0, "系统繁忙，请稍后重试。", backMainKeyboard())
			return true
		}
		b.updateMenu(chatID, userID, 0, "请输入用户名：", backMainKeyboard())
		return true
	}
	if err := b.db.Model(&model.Account{}).Where("category = ?", st.Old).Update("category", text).Error; err != nil {
		b.updateMenu(chatID, userID, 0, "分类更新失败。", backMainKeyboard())
		return true
	}
	b.updateMenu(chatID, userID, 0, "分类已更新。", backMainKeyboard())
	return true
}

func (b *TelegramBot) handleCallback(q *tgbotapi.CallbackQuery) {
	if q == nil || q.Message == nil || q.From == nil {
		return
	}
	chatID := q.Message.Chat.ID
	userID := fmt.Sprintf("%d", q.From.ID)
	isOperator := IsAllowed(b.cfg.AllowedUserIDs, userID)
	isBackupReceiver := IsAllowed(b.cfg.BackupReceiverIDs, userID)
	if !isOperator && !isBackupReceiver {
		b.audit(userID, "unauthorized", "")
		return
	}
	if q.Message.Chat != nil && q.Message.Chat.Type != "private" && !b.cfg.AllowGroupChat {
		_ = b.answerCallback(q, "请在私聊中使用该机器人")
		return
	}
	data := q.Data
	if isBackupReceiver && !isOperator {
		switch data {
		case "backup:ping":
			b.updateMenuNoDelete(chatID, userID, q.Message.MessageID, "连接正常。", backupMenuKeyboard())
		case "backup:run":
			b.updateMenuNoDelete(chatID, userID, q.Message.MessageID, "已开始备份，完成后将发送到接收人。", backupMenuKeyboard())
			go b.runManualBackup(chatID, userID, false)
		case "backup:test":
			b.updateMenuNoDelete(chatID, userID, q.Message.MessageID, "开始备份测试...", backupMenuKeyboard())
			go b.runBackupTest(chatID, userID)
		case "backup:help":
			b.sendBackupHelp(chatID, userID, q.Message.MessageID)
		}
		_ = b.answerCallback(q, "")
		return
	}
	switch {
	case data == "menu:find":
		if !b.requireUnlockedForQuery(chatID, userID, q.Message.MessageID) {
			break
		}
		b.sendCategoryMenu(chatID, userID, q.Message.MessageID)
	case data == "menu:search":
		if !b.requireUnlockedForQuery(chatID, userID, q.Message.MessageID) {
			break
		}
		b.sendSearchFieldMenu(chatID, userID, q.Message.MessageID)
	case data == "menu:ttl":
		b.sendTTLMenu(chatID, userID, q.Message.MessageID)
	case data == "menu:backup":
		if strings.TrimSpace(b.cfg.BackupPassword) == "" || len(b.cfg.BackupReceiverIDs) == 0 {
			b.updateMenu(chatID, userID, q.Message.MessageID, "备份未配置，请设置 BACKUP_PASSWORD 与 BACKUP_RECEIVER_IDS。", backMainKeyboard())
			break
		}
		b.updateMenu(chatID, userID, q.Message.MessageID, "已开始备份，完成后会发送到备份接收人。", backMainKeyboard())
		go b.runManualBackup(chatID, userID, true)
	case data == "menu:help":
		b.sendHelpMenu(chatID, userID, q.Message.MessageID)
	case data == "menu:add":
		b.updateMenu(chatID, userID, q.Message.MessageID, "请输入 /add 开始新增账号。", backMainKeyboard())
	case strings.HasPrefix(data, "ttl:"):
		if b.store != nil {
			seconds, err := strconv.Atoi(strings.TrimPrefix(data, "ttl:"))
			if err == nil && seconds > 0 {
				_ = b.store.Set(context.Background(), "tg:ttl:"+userID, strconv.Itoa(seconds), 30*24*time.Hour)
				b.updateMenu(chatID, userID, q.Message.MessageID, "已设置自动删除时间为 "+strconv.Itoa(seconds/60)+" 分钟。", backMainKeyboard())
			}
		}
	case strings.HasPrefix(data, "searchfield:"):
		field := strings.TrimPrefix(data, "searchfield:")
		if b.store != nil {
			_ = saveSearchState(context.Background(), b.store, stateKey("tg:search", userID), &searchState{Field: field}, searchTTL)
		}
		b.updateMenu(chatID, userID, q.Message.MessageID, "请输入关键词：", backMainKeyboard())
	case strings.HasPrefix(data, "cat:"):
		if !b.requireUnlockedForQuery(chatID, userID, q.Message.MessageID) {
			break
		}
		if b.store == nil {
			b.reply(chatID, "会话存储不可用，请检查 Redis 配置。")
			break
		}
		idxStr := strings.TrimPrefix(data, "cat:")
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			break
		}
		st, err := loadCategoryState(context.Background(), b.store, stateKey("tg:catmap", userID))
		if err != nil || st == nil || idx < 0 || idx >= len(st.Categories) {
			break
		}
		category := st.Categories[idx]
		_ = b.store.Set(context.Background(), stateKey("tg:lastcat", userID), category, menuTTL)
		var accounts []model.Account
		if err := b.db.Where("category = ?", category).Order("platform").Find(&accounts).Error; err != nil {
			b.updateMenu(chatID, userID, q.Message.MessageID, "查询失败。", backMainKeyboard())
			break
		}
		if len(accounts) == 0 {
			b.updateMenu(chatID, userID, q.Message.MessageID, "该分类暂无记录。", backMainKeyboard())
			break
		}
		b.sendAccountsMenu(chatID, userID, q.Message.MessageID, "请选择平台：", accounts)
	case strings.HasPrefix(data, "cat_edit:"):
		if b.store == nil {
			b.reply(chatID, "会话存储不可用，请检查 Redis 配置。")
			break
		}
		idxStr := strings.TrimPrefix(data, "cat_edit:")
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			break
		}
		st, err := loadCategoryState(context.Background(), b.store, stateKey("tg:catmap", userID))
		if err != nil || st == nil || idx < 0 || idx >= len(st.Categories) {
			break
		}
		old := st.Categories[idx]
		_ = saveCategoryEditState(context.Background(), b.store, stateKey("tg:cat_edit", userID), &categoryEditState{Mode: "edit", Old: old}, editTTL)
		b.updateMenu(chatID, userID, q.Message.MessageID, "请输入新的分类名称：", backMainKeyboard())
	case strings.HasPrefix(data, "cat_del:"):
		if b.store == nil {
			b.reply(chatID, "会话存储不可用，请检查 Redis 配置。")
			break
		}
		idxStr := strings.TrimPrefix(data, "cat_del:")
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			break
		}
		st, err := loadCategoryState(context.Background(), b.store, stateKey("tg:catmap", userID))
		if err != nil || st == nil || idx < 0 || idx >= len(st.Categories) {
			break
		}
		old := st.Categories[idx]
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("确认删除分类", "cat_delconfirm:"+strconv.Itoa(idx)),
				tgbotapi.NewInlineKeyboardButtonData("取消", "back:categories"),
			),
		)
		b.updateMenu(chatID, userID, q.Message.MessageID, "确认删除分类 \""+old+"\"？将删除该分类下全部记录。", keyboard)
	case strings.HasPrefix(data, "cat_delconfirm:"):
		if b.store == nil {
			b.reply(chatID, "会话存储不可用，请检查 Redis 配置。")
			break
		}
		idxStr := strings.TrimPrefix(data, "cat_delconfirm:")
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			break
		}
		st, err := loadCategoryState(context.Background(), b.store, stateKey("tg:catmap", userID))
		if err != nil || st == nil || idx < 0 || idx >= len(st.Categories) {
			break
		}
		old := st.Categories[idx]
		if err := b.db.Where("category = ?", old).Delete(&model.Account{}).Error; err != nil {
			b.updateMenu(chatID, userID, q.Message.MessageID, "删除失败。", backMainKeyboard())
		} else {
			b.updateMenu(chatID, userID, q.Message.MessageID, "分类已删除。", backMainKeyboard())
		}
	case data == "cat_add":
		if b.store == nil {
			b.reply(chatID, "会话存储不可用，请检查 Redis 配置。")
			break
		}
		_ = saveCategoryEditState(context.Background(), b.store, stateKey("tg:cat_edit", userID), &categoryEditState{Mode: "add"}, editTTL)
		b.updateMenu(chatID, userID, q.Message.MessageID, "请输入新的分类名称：", backMainKeyboard())
	case strings.HasPrefix(data, "acct:"):
		id := strings.TrimPrefix(data, "acct:")
		b.sendAccountDetail(chatID, userID, q.Message.MessageID, id)
	case strings.HasPrefix(data, "copy:"):
		parts := strings.SplitN(strings.TrimPrefix(data, "copy:"), ":", 2)
		if len(parts) != 2 {
			break
		}
		field, id := parts[0], parts[1]
		b.sendCopyValue(chatID, userID, id, field)
	case strings.HasPrefix(data, "edit:"):
		id := strings.TrimPrefix(data, "edit:")
		b.sendEditFieldMenu(chatID, userID, q.Message.MessageID, id)
	case strings.HasPrefix(data, "editfield:"):
		parts := strings.SplitN(strings.TrimPrefix(data, "editfield:"), ":", 2)
		if len(parts) != 2 {
			break
		}
		field, id := parts[0], parts[1]
		if b.store != nil {
			_ = saveEditState(context.Background(), b.store, stateKey("tg:edit", userID), &editState{AccountID: id, Field: field}, editTTL)
		}
		b.updateMenu(chatID, userID, q.Message.MessageID, "请输入新的值：", backMainKeyboard())
	case strings.HasPrefix(data, "del:"):
		id := strings.TrimPrefix(data, "del:")
		b.sendDeleteConfirm(chatID, userID, q.Message.MessageID, id)
	case strings.HasPrefix(data, "delconfirm:"):
		id := strings.TrimPrefix(data, "delconfirm:")
		var account model.Account
		if err := b.db.First(&account, "id = ?", id).Error; err != nil {
			b.updateMenu(chatID, userID, q.Message.MessageID, "记录不存在。", backMainKeyboard())
			break
		}
		if err := b.db.Delete(&model.Account{}, "id = ?", id).Error; err != nil {
			b.updateMenu(chatID, userID, q.Message.MessageID, "删除失败。", backMainKeyboard())
		} else {
			b.audit(userID, "delete", account.Platform)
			b.updateMenu(chatID, userID, q.Message.MessageID, "已删除。", backMainKeyboard())
		}
	case data == "back:categories":
		if !b.requireUnlockedForQuery(chatID, userID, q.Message.MessageID) {
			break
		}
		b.sendCategoryMenu(chatID, userID, q.Message.MessageID)
	case data == "back:main":
		b.sendMainMenu(chatID, userID, q.Message.MessageID)
	}
	_ = b.answerCallback(q, "")
}

func (b *TelegramBot) sendAccountDetail(chatID int64, userID string, messageID int, id string) {
	if !b.requireUnlocked(chatID, userID) {
		return
	}
	var account model.Account
	if err := b.db.First(&account, "id = ?", id).Error; err != nil {
		b.updateMenu(chatID, userID, messageID, "记录不存在。", backMainKeyboard())
		return
	}
	pwd, usedLegacy, err := crypto.DecryptWithFallback(account.EncryptedPassword, account.Nonce, b.derivedKey, b.legacyKey)
	if err != nil {
		b.updateMenu(chatID, userID, messageID, "解密失败。", backMainKeyboard())
		return
	}
	if usedLegacy {
		if ciphertext, nonce, err := crypto.Encrypt(pwd, b.derivedKey); err == nil {
			_ = b.db.Model(&account).Updates(map[string]any{
				"encrypted_password": ciphertext,
				"nonce":              nonce,
			}).Error
		}
	}
	text := fmt.Sprintf("平台: %s\n分类: %s\n用户名: %s\n密码: %s\n邮箱: %s\n手机: %s\n备注: %s",
		account.Platform, account.Category, account.Username, pwd, account.Email, account.Phone, account.Notes)
	buttons := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("复制用户名", "copy:username:"+account.ID.String()),
		tgbotapi.NewInlineKeyboardButtonData("复制密码", "copy:password:"+account.ID.String()),
	}
	if account.Email != "" {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("复制邮箱", "copy:email:"+account.ID.String()))
	}
	if account.Phone != "" {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("复制手机号", "copy:phone:"+account.ID.String()))
	}
	if account.Notes != "" {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData("复制备注", "copy:notes:"+account.ID.String()))
	}
	actionRow := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("修改", "edit:"+account.ID.String()),
		tgbotapi.NewInlineKeyboardButtonData("删除", "del:"+account.ID.String()),
		tgbotapi.NewInlineKeyboardButtonData("返回分类", "back:categories"),
	}
	keyboard := buildInlineKeyboard(buttons, 2)
	keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, actionRow)
	b.updateMenu(chatID, userID, messageID, text, keyboard)
	b.deleteMenuAfter(chatID, userID, messageID, passwordQueryTTL)
}

func (b *TelegramBot) sendCopyValue(chatID int64, userID string, id string, field string) {
	var account model.Account
	if err := b.db.First(&account, "id = ?", id).Error; err != nil {
		b.reply(chatID, "记录不存在。")
		return
	}
	var value string
	switch field {
	case "username":
		value = account.Username
	case "password":
		if !b.requireUnlocked(chatID, userID) {
			return
		}
		pwd, usedLegacy, err := crypto.DecryptWithFallback(account.EncryptedPassword, account.Nonce, b.derivedKey, b.legacyKey)
		if err != nil {
			b.reply(chatID, "解密失败。")
			return
		}
		if usedLegacy {
			if ciphertext, nonce, err := crypto.Encrypt(pwd, b.derivedKey); err == nil {
				_ = b.db.Model(&account).Updates(map[string]any{
					"encrypted_password": ciphertext,
					"nonce":              nonce,
				}).Error
			}
		}
		value = pwd
	case "email":
		value = account.Email
	case "phone":
		value = account.Phone
	case "notes":
		value = account.Notes
	default:
		b.reply(chatID, "不支持的字段。")
		return
	}
	if value == "" {
		b.reply(chatID, "该字段为空。")
		return
	}
	msg := tgbotapi.NewMessage(chatID, value)
	sent, err := b.bot.Send(msg)
	if err != nil {
		log.Printf("telegram send failed chat_id=%d err=%v", chatID, err)
		return
	}
	if field == "password" {
		b.deleteMessageAfter(chatID, sent.MessageID, passwordQueryTTL)
	} else {
		b.deleteMessageLaterForUser(chatID, sent.MessageID, userID)
	}
}

func (b *TelegramBot) sendEditFieldMenu(chatID int64, userID string, messageID int, id string) {
	buttons := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("平台", "editfield:platform:"+id),
		tgbotapi.NewInlineKeyboardButtonData("分类", "editfield:category:"+id),
		tgbotapi.NewInlineKeyboardButtonData("用户名", "editfield:username:"+id),
		tgbotapi.NewInlineKeyboardButtonData("密码", "editfield:password:"+id),
		tgbotapi.NewInlineKeyboardButtonData("邮箱", "editfield:email:"+id),
		tgbotapi.NewInlineKeyboardButtonData("手机号", "editfield:phone:"+id),
		tgbotapi.NewInlineKeyboardButtonData("备注", "editfield:notes:"+id),
		tgbotapi.NewInlineKeyboardButtonData("返回分类", "back:categories"),
	}
	keyboard := buildInlineKeyboard(buttons, 2)
	b.updateMenu(chatID, userID, messageID, "请选择需要修改的字段：", keyboard)
}

func (b *TelegramBot) runManualBackup(chatID int64, userID string, autoDelete bool) {
	if strings.TrimSpace(b.cfg.BackupPassword) == "" || len(b.cfg.BackupReceiverIDs) == 0 {
		b.sendBackupReceipt(chatID, userID, "备份未配置，请设置 BACKUP_PASSWORD 与 BACKUP_RECEIVER_IDS。", autoDelete)
		return
	}
	err := RunBackupNow(b.ctx, b.bot, b.cfg, b.db, userID)
	if err != nil {
		b.sendBackupReceipt(chatID, userID, "备份失败："+err.Error(), autoDelete)
		return
	}
	b.sendBackupReceipt(chatID, userID, "备份成功，已发送到备份接收人。", autoDelete)
}

func (b *TelegramBot) runBackupTest(chatID int64, userID string) {
	if strings.TrimSpace(b.cfg.BackupPassword) == "" || len(b.cfg.BackupReceiverIDs) == 0 {
		b.sendBackupReceipt(chatID, userID, "备份未配置，请设置 BACKUP_PASSWORD 与 BACKUP_RECEIVER_IDS。", false)
		return
	}
	err := RunBackupTest(b.ctx, b.cfg)
	if err != nil {
		b.sendBackupReceipt(chatID, userID, "备份测试失败："+err.Error(), false)
		return
	}
	b.sendBackupReceipt(chatID, userID, "备份测试成功。", false)
}

func (b *TelegramBot) sendBackupReceipt(chatID int64, userID string, text string, autoDelete bool) {
	msg := tgbotapi.NewMessage(chatID, text)
	sent, err := b.bot.Send(msg)
	if err != nil {
		log.Printf("telegram send failed chat_id=%d err=%v", chatID, err)
		return
	}
	if autoDelete {
		b.deleteMessageLaterForUser(chatID, sent.MessageID, userID)
	}
}

func (b *TelegramBot) sendDeleteConfirm(chatID int64, userID string, messageID int, id string) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("确认删除", "delconfirm:"+id),
			tgbotapi.NewInlineKeyboardButtonData("取消", "back:categories"),
		),
	)
	b.updateMenu(chatID, userID, messageID, "确认删除该记录？", keyboard)
}

func (b *TelegramBot) sendTTLMenu(chatID int64, userID string, messageID int) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("3 分钟", "ttl:180"),
			tgbotapi.NewInlineKeyboardButtonData("5 分钟", "ttl:300"),
			tgbotapi.NewInlineKeyboardButtonData("10 分钟", "ttl:600"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回主菜单", "back:main"),
		),
	)
	b.updateMenu(chatID, userID, messageID, "请选择自动删除时间：", keyboard)
}

func (b *TelegramBot) answerCallback(q *tgbotapi.CallbackQuery, text string) error {
	cfg := tgbotapi.NewCallback(q.ID, text)
	_, err := b.bot.Request(cfg)
	return err
}

func buildInlineKeyboard(buttons []tgbotapi.InlineKeyboardButton, perRow int) tgbotapi.InlineKeyboardMarkup {
	if perRow <= 0 {
		perRow = 2
	}
	rows := [][]tgbotapi.InlineKeyboardButton{}
	for i := 0; i < len(buttons); i += perRow {
		end := i + perRow
		if end > len(buttons) {
			end = len(buttons)
		}
		rows = append(rows, buttons[i:end])
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (b *TelegramBot) sendHelpMenu(chatID int64, userID string, messageID int) {
	unlockLine := "/unlock <PIN> - 解锁密码查询（15 分钟有效）"
	if isSixDigitPIN(b.cfg.UnlockPIN) {
		unlockLine = "/unlock <PIN> - 解锁密码查询（15 分钟有效，PIN 为 6 位数字）"
	}
	help := "可用指令说明：\n" +
		"/menu - 打开主菜单\n" +
		"/start - 显示功能入口\n" +
		unlockLine + "\n" +
		"/add - 新增账号（引导式输入）\n" +
		"/find <platform> - 按平台关键词查询（无参数进入分类浏览）\n" +
		"/search - 按字段搜索\n" +
		"/list - 按分类浏览\n" +
		"/ttl - 设置自动删除时间\n" +
		"手动备份 - 在主菜单点击\n" +
		"/cancel - 取消当前引导流程\n" +
		"/help - 显示帮助"
	b.updateMenu(chatID, userID, messageID, help, backMainKeyboard())
}

func (b *TelegramBot) sendBackupHelp(chatID int64, userID string, messageID int) {
	help := "备份接收人指令：\n" +
		"/menu - 打开备份菜单\n" +
		"/start - 显示备份菜单\n" +
		"/ping - 连接测试\n" +
		"/backup - 手动触发备份\n" +
		"/backup_test - 备份流程测试\n" +
		"/help - 帮助说明"
	b.updateMenuNoDelete(chatID, userID, messageID, help, backupMenuKeyboard())
}

func (b *TelegramBot) updateMenu(chatID int64, userID string, messageID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	if messageID > 0 {
		if b.editMenuMessage(chatID, messageID, text, keyboard) == nil {
			b.setMenuMsgID(userID, messageID)
			b.deleteMessageLaterForUser(chatID, messageID, userID)
			return
		}
	}
	if stored := b.getMenuMsgID(userID); stored > 0 {
		if b.editMenuMessage(chatID, stored, text, keyboard) == nil {
			b.deleteMessageLaterForUser(chatID, stored, userID)
			return
		}
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	sent, err := b.bot.Send(msg)
	if err != nil {
		log.Printf("telegram send failed chat_id=%d err=%v", chatID, err)
		return
	}
	b.setMenuMsgID(userID, sent.MessageID)
	b.deleteMessageLaterForUser(chatID, sent.MessageID, userID)
}

func (b *TelegramBot) updateMenuNoDelete(chatID int64, userID string, messageID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	if messageID > 0 {
		if b.editMenuMessage(chatID, messageID, text, keyboard) == nil {
			b.setMenuMsgID(userID, messageID)
			return
		}
	}
	if stored := b.getMenuMsgID(userID); stored > 0 {
		if b.editMenuMessage(chatID, stored, text, keyboard) == nil {
			return
		}
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	sent, err := b.bot.Send(msg)
	if err != nil {
		log.Printf("telegram send failed chat_id=%d err=%v", chatID, err)
		return
	}
	b.setMenuMsgID(userID, sent.MessageID)
}

func (b *TelegramBot) deleteMenuAfter(chatID int64, userID string, messageID int, delay time.Duration) {
	if delay <= 0 {
		return
	}
	if messageID > 0 {
		b.deleteMessageAfter(chatID, messageID, delay)
		return
	}
	if stored := b.getMenuMsgID(userID); stored > 0 {
		b.deleteMessageAfter(chatID, stored, delay)
	}
}

func (b *TelegramBot) editMenuMessage(chatID int64, messageID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) error {
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ReplyMarkup = &keyboard
	_, err := b.bot.Request(edit)
	return err
}

func (b *TelegramBot) ensureChatCommands(chatID int64, role string) {
	b.cmdMu.Lock()
	current := b.cmdScope[chatID]
	if current == role {
		b.cmdMu.Unlock()
		return
	}
	b.cmdScope[chatID] = role
	b.cmdMu.Unlock()

	switch role {
	case "backup":
		commands := []tgbotapi.BotCommand{
			{Command: "menu", Description: "打开备份菜单"},
			{Command: "start", Description: "显示备份菜单"},
			{Command: "ping", Description: "连接测试"},
			{Command: "backup", Description: "手动触发备份"},
			{Command: "backup_test", Description: "备份流程测试"},
			{Command: "help", Description: "帮助说明"},
		}
		scope := tgbotapi.NewBotCommandScopeChat(chatID)
		if _, err := b.bot.Request(tgbotapi.NewSetMyCommandsWithScope(scope, commands...)); err != nil {
			log.Printf("telegram set backup commands failed: %v", err)
		}
	default:
		scope := tgbotapi.NewBotCommandScopeChat(chatID)
		if _, err := b.bot.Request(tgbotapi.NewDeleteMyCommandsWithScope(scope)); err != nil {
			log.Printf("telegram delete chat commands failed: %v", err)
		}
	}
}

func (b *TelegramBot) getMenuMsgID(userID string) int {
	if b.store != nil {
		val, err := b.store.Get(context.Background(), "tg:menu:msg:"+userID)
		if err == nil && val != "" {
			if id, err := strconv.Atoi(val); err == nil {
				return id
			}
		}
	}
	b.menuMu.Lock()
	defer b.menuMu.Unlock()
	return b.menuMsg[userID]
}

func (b *TelegramBot) setMenuMsgID(userID string, messageID int) {
	if b.store != nil {
		_ = b.store.Set(context.Background(), "tg:menu:msg:"+userID, strconv.Itoa(messageID), 24*time.Hour)
	}
	b.menuMu.Lock()
	defer b.menuMu.Unlock()
	b.menuMsg[userID] = messageID
}

func (b *TelegramBot) audit(userID string, action string, platform string) {
	if action == "" {
		return
	}
	entry := &model.AuditLog{
		UserID:   userID,
		Action:   action,
		Platform: platform,
	}
	if err := b.db.Create(entry).Error; err != nil {
		log.Printf("audit log failed action=%s user_id=%s err=%v", action, userID, err)
	}
}

func backMainKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回主菜单", "back:main"),
		),
	)
}
