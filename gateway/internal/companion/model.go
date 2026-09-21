// Package companion owns consented phone pairing and bounded remote commands.
package companion

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

var (
	ErrDenied  = errors.New("companion_denied")
	ErrExpired = errors.New("companion_expired")
	ErrBusy    = errors.New("companion_busy")
	ErrInvalid = errors.New("companion_invalid")
)

type Persistence interface {
	Get(context.Context, string, string, any) error
	PutMany(context.Context, ...domain.Record) error
	List(context.Context, string) ([]json.RawMessage, error)
	Delete(context.Context, string, string) error
}

type Invitation struct {
	ID         string    `json:"id"`
	TargetID   string    `json:"targetId"`
	TargetName string    `json:"targetName"`
	Secret     string    `json:"secret"`
	Code       string    `json:"code"`
	Expires    time.Time `json:"expires"`
	Used       bool      `json:"used"`
}

type Request struct {
	ID         string    `json:"id"`
	TargetID   string    `json:"targetId"`
	Name       string    `json:"name"`
	Comparison string    `json:"comparison"`
	State      string    `json:"state"`
	Expires    time.Time `json:"expires"`
	// Only hashes are persisted. Secret is returned to the initiating phone once.
	TokenHash string `json:"tokenHash,omitempty"`
}

type Grant struct {
	ID         string    `json:"id"`
	TargetID   string    `json:"targetId"`
	TargetName string    `json:"targetName"`
	Name       string    `json:"name"`
	TokenHash  string    `json:"tokenHash,omitempty"`
	Created    time.Time `json:"created"`
}

type Join struct {
	InvitationID string `json:"invitationId"`
	Secret       string `json:"secret"`
	Code         string `json:"code"`
	Name         string `json:"name"`
}

type Command struct {
	ID          string    `json:"id"`
	GrantID     string    `json:"-"`
	Action      string    `json:"action"`
	Provider    string    `json:"provider,omitempty"`
	Expires     time.Time `json:"-"`
	RemainingMS int64     `json:"remainingMs"`
}

type Result struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}
