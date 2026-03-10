package bot

import (
	"context"
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
	bot       *tgbotapi.BotAPI
	cfg       *config.Config
	db        *gorm.DB
	store     *store.RedisStore
	masterKey []byte
	ctx       context.Context
	menuMu    sync.Mutex
	menuMsg   map[string]int
}

const (
	searchTTL = 10 * time.Minute
	editTTL   = 10 * time.Minute
	menuTTL   = 10 * time.Minute
)

// StartTelegramBot initializes the bot and starts the update loop.
func StartTelegramBot(ctx context.Context, cfg *config.Config, db *gorm.DB, store *store.RedisStore, masterKey []byte) (*TelegramBot, error) {
	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required to start Telegram bot")
	}
	bot, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, err
	}
	if _, err := bot.Request(tgbotapi.NewDeleteMyCommands()); err != nil {
		log.Printf("telegram delete commands failed: %v", err)
	}
	b := &TelegramBot{
		bot:       bot,
		cfg:       cfg,
		db:        db,
		store:     store,
		masterKey: masterKey,
		ctx:       ctx,
		menuMsg:   make(map[string]int),
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
	if msg.Chat != nil && msg.Chat.Type != "private" && !b.cfg.AllowGroupChat {
		b.reply(msg.Chat.ID, "请在私聊中使用该机器人。")
		return
	}
	userID := fmt.Sprintf("%d", msg.From.ID)
	if msg.MessageID != 0 {
		b.deleteMessageLaterForUser(msg.Chat.ID, msg.MessageID, userID)
	}
	if !IsAllowed(b.cfg.AllowedUserIDs, userID) {
		b.reply(msg.Chat.ID, "无权限访问此机器人。")
		return
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
		case "add":
			b.startAddFlow(msg.Chat.ID, userID)
		case "help":
			b.sendHelpMenu(msg.Chat.ID, userID, 0)
		case "find":
			query := strings.TrimSpace(msg.CommandArguments())
			if query == "" {
				b.sendCategoryMenu(msg.Chat.ID, userID, 0)
				return
			}
			b.handleFind(msg.Chat.ID, userID, query)
		case "search":
			b.sendSearchFieldMenu(msg.Chat.ID, userID, 0)
		case "ttl":
			b.sendTTLMenu(msg.Chat.ID, userID, 0)
		case "list":
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
		ciphertext, nonce, err := crypto.Encrypt(text, b.masterKey)
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
	_ = b.store.Del(context.Background(), stateKey("tg:add", userID))
	b.reply(chatID, "已保存。")
}

func (b *TelegramBot) handleFind(chatID int64, userID string, query string) {
	if query == "" {
		b.updateMenu(chatID, userID, 0, "请输入平台关键词，例如：/find github", backMainKeyboard())
		return
	}
	var accounts []model.Account
	if err := b.db.Where("platform ILIKE ?", "%"+query+"%").Order("platform").Find(&accounts).Error; err != nil {
		b.updateMenu(chatID, userID, 0, "查询失败。", backMainKeyboard())
		return
	}
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
		tgbotapi.NewInlineKeyboardButtonData("帮助", "menu:help"),
	}
	keyboard := buildInlineKeyboard(buttons, 2)
	b.updateMenu(chatID, userID, messageID, text, keyboard)
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
		ciphertext, nonce, err := crypto.Encrypt(text, b.masterKey)
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
	if !IsAllowed(b.cfg.AllowedUserIDs, userID) {
		_ = b.answerCallback(q, "无权限")
		return
	}
	if q.Message.Chat != nil && q.Message.Chat.Type != "private" && !b.cfg.AllowGroupChat {
		_ = b.answerCallback(q, "请在私聊中使用该机器人")
		return
	}
	data := q.Data
	switch {
	case data == "menu:find":
		b.sendCategoryMenu(chatID, userID, q.Message.MessageID)
	case data == "menu:search":
		b.sendSearchFieldMenu(chatID, userID, q.Message.MessageID)
	case data == "menu:ttl":
		b.sendTTLMenu(chatID, userID, q.Message.MessageID)
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
		if err := b.db.Delete(&model.Account{}, "id = ?", id).Error; err != nil {
			b.updateMenu(chatID, userID, q.Message.MessageID, "删除失败。", backMainKeyboard())
		} else {
			b.updateMenu(chatID, userID, q.Message.MessageID, "已删除。", backMainKeyboard())
		}
	case data == "back:categories":
		b.sendCategoryMenu(chatID, userID, q.Message.MessageID)
	case data == "back:main":
		b.sendMainMenu(chatID, userID, q.Message.MessageID)
	}
	_ = b.answerCallback(q, "")
}

func (b *TelegramBot) sendAccountDetail(chatID int64, userID string, messageID int, id string) {
	var account model.Account
	if err := b.db.First(&account, "id = ?", id).Error; err != nil {
		b.updateMenu(chatID, userID, messageID, "记录不存在。", backMainKeyboard())
		return
	}
	pwd, err := crypto.Decrypt(account.EncryptedPassword, account.Nonce, b.masterKey)
	if err != nil {
		b.updateMenu(chatID, userID, messageID, "解密失败。", backMainKeyboard())
		return
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
		pwd, err := crypto.Decrypt(account.EncryptedPassword, account.Nonce, b.masterKey)
		if err != nil {
			b.reply(chatID, "解密失败。")
			return
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
	b.deleteMessageLaterForUser(chatID, sent.MessageID, userID)
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
	help := "可用指令说明：\n" +
		"/menu - 打开主菜单\n" +
		"/start - 显示功能入口\n" +
		"/add - 新增账号（引导式输入）\n" +
		"/find <platform> - 按平台关键词查询（无参数进入分类浏览）\n" +
		"/search - 按字段搜索\n" +
		"/list - 按分类浏览\n" +
		"/ttl - 设置自动删除时间\n" +
		"/cancel - 取消当前引导流程\n" +
		"/help - 显示帮助"
	b.updateMenu(chatID, userID, messageID, help, backMainKeyboard())
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

func (b *TelegramBot) editMenuMessage(chatID int64, messageID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) error {
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ReplyMarkup = &keyboard
	_, err := b.bot.Request(edit)
	return err
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

func backMainKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回主菜单", "back:main"),
		),
	)
}
