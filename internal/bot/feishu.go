package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"vaultbot/internal/config"
	"vaultbot/internal/crypto"
	"vaultbot/internal/model"
	"vaultbot/internal/store"

	"github.com/gin-gonic/gin"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"gorm.io/gorm"
)

// FeishuBot handles Feishu events.
type FeishuBot struct {
	client    *lark.Client
	cfg       *config.Config
	db        *gorm.DB
	store     *store.RedisStore
	masterKey []byte
}

func NewFeishuBot(cfg *config.Config, db *gorm.DB, store *store.RedisStore, masterKey []byte) (*FeishuBot, error) {
	if cfg.FeishuAppID == "" || cfg.FeishuAppSecret == "" {
		return nil, fmt.Errorf("FEISHU_APP_ID/FEISHU_APP_SECRET required to start Feishu bot")
	}
	client := lark.NewClient(cfg.FeishuAppID, cfg.FeishuAppSecret)
	return &FeishuBot{client: client, cfg: cfg, db: db, store: store, masterKey: masterKey}, nil
}

// HandleEvent handles Feishu event callbacks.
func (b *FeishuBot) HandleEvent(c *gin.Context) {
	body, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid payload"})
		return
	}
	if b.cfg.FeishuEncryptKey != "" {
		if !verifyLarkSignature(c, body, b.cfg.FeishuEncryptKey) {
			c.JSON(401, gin.H{"error": "invalid signature"})
			return
		}
	}

	var callback feishuCallback
	plaintext := body
	if encrypted := extractEncryptField(body); encrypted != "" {
		if b.cfg.FeishuEncryptKey == "" {
			c.JSON(401, gin.H{"error": "encrypt key required"})
			return
		}
		decoded, err := decryptLarkEvent(b.cfg.FeishuEncryptKey, encrypted)
		if err != nil {
			c.JSON(400, gin.H{"error": "decrypt failed"})
			return
		}
		plaintext = decoded
	}

	if err := json.Unmarshal(plaintext, &callback); err != nil {
		c.JSON(400, gin.H{"error": "invalid payload"})
		return
	}
	if b.cfg.FeishuVerificationToken != "" && callback.Token != "" && callback.Token != b.cfg.FeishuVerificationToken {
		c.JSON(401, gin.H{"error": "invalid token"})
		return
	}
	if callback.Challenge != "" {
		c.JSON(200, gin.H{"challenge": callback.Challenge})
		return
	}
	if callback.Event == nil {
		c.JSON(200, gin.H{"ok": true})
		return
	}

	senderID := callback.Event.Sender.SenderID.UserID
	if senderID == "" {
		senderID = callback.Event.Sender.SenderID.OpenID
	}
	if !IsAllowed(b.cfg.AllowedUserIDs, senderID) {
		c.JSON(200, gin.H{"ok": true})
		return
	}
	allowed, err := b.store.Allow(context.Background(), "rate:lark:"+senderID, 20, time.Minute)
	if err != nil || !allowed {
		c.JSON(200, gin.H{"ok": true})
		return
	}

	contentText := parseFeishuText(callback.Event.Message.Content)
	contentText = strings.TrimSpace(contentText)
	if contentText == "" {
		c.JSON(200, gin.H{"ok": true})
		return
	}

	chatID := callback.Event.Message.ChatID
	if callback.Event.Message.ChatType != "" && callback.Event.Message.ChatType != "p2p" && !b.cfg.AllowGroupChat {
		_ = b.sendText(chatID, "请在私聊中使用该机器人。")
		c.JSON(200, gin.H{"ok": true})
		return
	}
	if strings.HasPrefix(contentText, "/add") {
		b.startAddFlow(chatID, senderID)
	} else if strings.HasPrefix(contentText, "/find") {
		query := strings.TrimSpace(strings.TrimPrefix(contentText, "/find"))
		_ = b.store.Del(context.Background(), stateKey("lark:add", senderID))
		b.handleFind(chatID, query)
	} else if strings.HasPrefix(contentText, "/list") {
		_ = b.store.Del(context.Background(), stateKey("lark:add", senderID))
		b.handleList(chatID)
	} else if strings.HasPrefix(contentText, "/cancel") {
		_ = b.store.Del(context.Background(), stateKey("lark:add", senderID))
		_ = b.sendText(chatID, "已取消当前操作。")
	} else {
		// handle add flow message
		b.handleAddStep(chatID, senderID, contentText)
	}

	c.JSON(200, gin.H{"ok": true})
}

type feishuCallback struct {
	Challenge string       `json:"challenge"`
	Type      string       `json:"type"`
	Token     string       `json:"token"`
	Event     *feishuEvent `json:"event"`
}

type feishuEvent struct {
	Message feishuMessage `json:"message"`
	Sender  feishuSender  `json:"sender"`
}

type feishuMessage struct {
	ChatID   string `json:"chat_id"`
	ChatType string `json:"chat_type"`
	Content  string `json:"content"`
}

type feishuSender struct {
	SenderID feishuSenderID `json:"sender_id"`
}

type feishuSenderID struct {
	UserID string `json:"user_id"`
	OpenID string `json:"open_id"`
}

func parseFeishuText(content string) string {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return ""
	}
	return payload.Text
}

func (b *FeishuBot) startAddFlow(chatID string, userID string) {
	st := &addState{Step: stepPlatform}
	_ = saveState(context.Background(), b.store, stateKey("lark:add", userID), st, 15*time.Minute)
	_ = b.sendText(chatID, "请输入平台名称：")
}

func (b *FeishuBot) handleAddStep(chatID string, userID string, text string) {
	key := stateKey("lark:add", userID)
	st, err := loadState(context.Background(), b.store, key)
	if err != nil || st == nil {
		return
	}

	switch st.Step {
	case stepPlatform:
		st.Platform = text
		st.Step = stepCategory
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
		_ = b.sendText(chatID, "请输入分类（如：工作/生活/金融）：")
	case stepCategory:
		st.Category = text
		st.Step = stepUsername
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
		_ = b.sendText(chatID, "请输入用户名：")
	case stepUsername:
		st.Username = text
		st.Step = stepPassword
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
		_ = b.sendText(chatID, "请输入密码（不会写入日志）：")
	case stepPassword:
		ciphertext, nonce, err := crypto.Encrypt(text, b.masterKey)
		if err != nil {
			_ = b.sendText(chatID, "加密失败，请稍后重试。")
			return
		}
		st.EncryptedPassword = ciphertext
		st.Nonce = nonce
		st.Step = stepEmail
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
		_ = b.sendText(chatID, "请输入邮箱（可输入 - 跳过）：")
	case stepEmail:
		if text == "-" {
			text = ""
		}
		st.Email = text
		st.Step = stepPhone
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
		_ = b.sendText(chatID, "请输入手机号（可输入 - 跳过）：")
	case stepPhone:
		if text == "-" {
			text = ""
		}
		st.Phone = text
		st.Step = stepNotes
		_ = saveState(context.Background(), b.store, key, st, 15*time.Minute)
		_ = b.sendText(chatID, "请输入备注（可输入 - 跳过）：")
	case stepNotes:
		if text == "-" {
			text = ""
		}
		st.Notes = text
		b.finishAddFlow(chatID, userID, st)
	}
}

func (b *FeishuBot) finishAddFlow(chatID string, userID string, st *addState) {
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
		_ = b.sendText(chatID, "保存失败，请稍后重试。")
		return
	}
	_ = b.store.Del(context.Background(), stateKey("lark:add", userID))
	_ = b.sendText(chatID, "已保存。")
}

func (b *FeishuBot) handleFind(chatID string, query string) {
	if query == "" {
		_ = b.sendText(chatID, "请输入平台关键词，例如：/find github")
		return
	}
	var accounts []model.Account
	if err := b.db.Where("platform ILIKE ?", "%"+query+"%").Order("platform").Find(&accounts).Error; err != nil {
		_ = b.sendText(chatID, "查询失败。")
		return
	}
	if len(accounts) == 0 {
		_ = b.sendText(chatID, "未找到记录。")
		return
	}
	for _, acc := range accounts {
		pwd, err := crypto.Decrypt(acc.EncryptedPassword, acc.Nonce, b.masterKey)
		if err != nil {
			_ = b.sendText(chatID, "解密失败。")
			continue
		}
		msgID, err := b.sendCard(chatID, acc, pwd)
		if err != nil {
			log.Printf("feishu send card failed chat_id=%s err=%v", chatID, err)
			continue
		}
		if msgID != "" {
			b.deleteMessageLater(msgID)
		}
	}
}

func (b *FeishuBot) handleList(chatID string) {
	var accounts []model.Account
	if err := b.db.Order("category").Order("platform").Find(&accounts).Error; err != nil {
		_ = b.sendText(chatID, "查询失败。")
		return
	}
	if len(accounts) == 0 {
		_ = b.sendText(chatID, "暂无记录。")
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
	_ = b.sendText(chatID, strings.TrimSpace(builder.String()))
}

func (b *FeishuBot) sendText(chatID string, text string) error {
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()
	_, _, err := b.client.Im.Message.Create(context.Background(), req)
	return err
}

func (b *FeishuBot) sendCard(chatID string, acc model.Account, password string) (string, error) {
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"elements": []any{
			map[string]any{
				"tag":  "div",
				"text": map[string]any{"tag": "lark_md", "content": fmt.Sprintf("**平台**: %s\n**分类**: %s\n**用户名**: %s\n**邮箱**: %s\n**手机**: %s\n**备注**: %s", acc.Platform, acc.Category, acc.Username, acc.Email, acc.Phone, acc.Notes)},
			},
			map[string]any{
				"tag":  "div",
				"text": map[string]any{"tag": "lark_md", "content": "**密码**: (点击复制按钮)"},
			},
			map[string]any{
				"tag": "action",
				"actions": []any{
					map[string]any{
						"tag":         "button",
						"text":        map[string]any{"tag": "plain_text", "content": "复制密码"},
						"type":        "primary",
						"action_type": "copy",
						"value":       map[string]any{"content": password},
					},
				},
			},
		},
	}
	content, _ := json.Marshal(card)
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("interactive").
			Content(string(content)).
			Build()).
		Build()
	resp, _, err := b.client.Im.Message.Create(context.Background(), req)
	if err != nil {
		return "", err
	}
	return resp.Data.MessageId, nil
}

func (b *FeishuBot) deleteMessageLater(messageID string) {
	go func() {
		timer := time.NewTimer(b.cfg.DeleteAfter)
		defer timer.Stop()
		<-timer.C
		b.deleteMessageWithRetry(messageID)
	}()
}

func (b *FeishuBot) deleteMessageWithRetry(messageID string) {
	const maxAttempts = 3
	for i := 0; i < maxAttempts; i++ {
		req := larkim.NewDeleteMessageReqBuilder().
			MessageId(messageID).
			Build()
		_, _, err := b.client.Im.Message.Delete(context.Background(), req)
		if err == nil {
			return
		}
		log.Printf("feishu delete failed message_id=%s attempt=%d err=%v", messageID, i+1, err)
		time.Sleep(time.Duration(i+1) * time.Second)
	}
}
