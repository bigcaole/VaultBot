package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AuditLog 记录关键操作的审计日志。
type AuditLog struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	UserID    string    `gorm:"size:64;index" json:"user_id"`
	Action    string    `gorm:"size:32;index" json:"action"`
	Platform  string    `gorm:"size:256;index" json:"platform"`
	CreatedAt time.Time `json:"created_at"`
}

func (a *AuditLog) BeforeCreate(tx *gorm.DB) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	return nil
}
