package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
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
}

// StartTelegramBot initializes the bot and starts the update loop.
func StartTelegramBot(ctx context.Context, cfg *config.Config, db *gorm.DB, store *store.RedisStore, masterKey []byte) (*TelegramBot, error) {
	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required to start Telegram bot")
	}
	bot, err := tgbotapi.NewBotAPI(cfg.TelegramBotToken)
	if err != nil {
		return nil, err
	}
	b := &TelegramBot{bot: bot, cfg: cfg, db: db, store: store, masterKey: masterKey, ctx: ctx}
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
	if !IsAllowed(b.cfg.AllowedUserIDs, userID) {
		b.reply(msg.Chat.ID, "无权限访问此机器人。")
		return
	}
	allowed, err := b.store.Allow(context.Background(), "rate:tg:"+userID, 20, time.Minute)
	if err != nil {
		b.reply(msg.Chat.ID, "系统繁忙，请稍后重试。")
		return
	}
	if !allowed {
		b.reply(msg.Chat.ID, "请求过于频繁，请稍后重试。")
		return
	}

	if msg.IsCommand() {
		if msg.Command() != "add" {
			_ = b.store.Del(context.Background(), stateKey("tg:add", userID))
		}
		switch msg.Command() {
		case "add":
			b.startAddFlow(msg.Chat.ID, userID)
		case "find":
			b.handleFind(msg.Chat.ID, strings.TrimSpace(msg.CommandArguments()))
		case "list":
			b.handleList(msg.Chat.ID)
		case "cancel":
			_ = b.store.Del(context.Background(), stateKey("tg:add", userID))
			b.reply(msg.Chat.ID, "已取消当前操作。")
		default:
			b.reply(msg.Chat.ID, "未知指令。可用指令：/add /find /list /cancel")
		}
		return
	}

	b.handleAddStep(msg.Chat.ID, userID, strings.TrimSpace(msg.Text))
}

func (b *TelegramBot) startAddFlow(chatID int64, userID string) {
	st := &addState{Step: stepPlatform}
	_ = saveState(context.Background(), b.store, stateKey("tg:add", userID), st, 15*time.Minute)
	b.reply(chatID, "请输入平台名称：")
}

func (b *TelegramBot) handleAddStep(chatID int64, userID string, text string) {
	key := stateKey("tg:add", userID)
	st, err := loadState(context.Background(), b.store, key)
	if err != nil || st == nil {
		return
	}

	switch st.Step {
	case stepPlatform:
		st.Platform = text
		st.Step = stepCategory
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
		b.reply(chatID, "请输入分类（如：工作/生活/金融）：")
	case stepCategory:
		st.Category = text
		st.Step = stepUsername
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
		b.reply(chatID, "请输入用户名：")
	case stepUsername:
		st.Username = text
		st.Step = stepPassword
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
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
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
		b.reply(chatID, "请输入邮箱（可输入 - 跳过）：")
	case stepEmail:
		if text == "-" {
			text = ""
		}
		st.Email = text
		st.Step = stepPhone
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
		b.reply(chatID, "请输入手机号（可输入 - 跳过）：")
	case stepPhone:
		if text == "-" {
			text = ""
		}
		st.Phone = text
		st.Step = stepNotes
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
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

func (b *TelegramBot) handleFind(chatID int64, query string) {
	if query == "" {
		b.reply(chatID, "请输入平台关键词，例如：/find github")
		return
	}
	var accounts []model.Account
	if err := b.db.Where("platform ILIKE ?", "%"+query+"%").Order("platform").Find(&accounts).Error; err != nil {
		b.reply(chatID, "查询失败。")
		return
	}
	if len(accounts) == 0 {
		b.reply(chatID, "未找到记录。")
		return
	}
	for _, acc := range accounts {
		pwd, err := crypto.Decrypt(acc.EncryptedPassword, acc.Nonce, b.masterKey)
		if err != nil {
			b.reply(chatID, "解密失败。")
			continue
		}
		text := fmt.Sprintf("平台: %s\n分类: %s\n用户名: %s\n密码: %s\n邮箱: %s\n手机: %s\n备注: %s", acc.Platform, acc.Category, acc.Username, pwd, acc.Email, acc.Phone, acc.Notes)
		sent, err := b.bot.Send(tgbotapi.NewMessage(chatID, text))
		if err != nil {
			log.Printf("telegram send failed chat_id=%d err=%v", chatID, err)
			continue
		}
		b.deleteMessageLater(chatID, sent.MessageID)
	}
}

func (b *TelegramBot) handleList(chatID int64) {
	var accounts []model.Account
	if err := b.db.Order("category").Order("platform").Find(&accounts).Error; err != nil {
		b.reply(chatID, "查询失败。")
		return
	}
	if len(accounts) == 0 {
		b.reply(chatID, "暂无记录。")
		return
	}
	builder := &strings.Builder{}
	currentCategory := ""
	for _, acc := range accounts {
		if acc.Category != currentCategory {
			currentCategory = acc.Category
			builder.WriteString("\n[" + currentCategory + "]\n")
		}
		builder.WriteString(fmt.Sprintf("- %s (%s)\n", acc.Platform, acc.Username))
	}
	b.reply(chatID, strings.TrimSpace(builder.String()))
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
