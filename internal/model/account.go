package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Account 存储加密后的账号信息。
type Account struct {
	ID                uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	Platform          string    `gorm:"size:128;index" json:"platform"`
	Category          string    `gorm:"size:128;index" json:"category"`
	Username          string    `gorm:"size:128;index" json:"username"`
	EncryptedPassword string    `gorm:"type:text" json:"encrypted_password"`
	Email             string    `gorm:"size:256" json:"email"`
	Phone             string    `gorm:"size:64" json:"phone"`
	Notes             string    `gorm:"type:text" json:"notes"`
	Nonce             string    `gorm:"type:text" json:"nonce"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (a *Account) BeforeCreate(tx *gorm.DB) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	return nil
}
