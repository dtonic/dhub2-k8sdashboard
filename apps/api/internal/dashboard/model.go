package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	SchemaVersion      = 1
	MaxDraftsPerOwner  = 32
	MaxDefinitionBytes = 65536
	MaxListPage        = 50
)

type State string

const (
	StateDraft     State = "draft"
	StateSubmitted State = "submitted"
	StateApproved  State = "approved"
)

type Layout struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}
type Variable struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}
type Options struct {
	MaxRows int `json:"maxRows"`
}
type Widget struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Type      string   `json:"type"`
	Binding   string   `json:"binding"`
	QueryRefs []string `json:"queryRefs,omitempty"`
	Options   *Options `json:"options,omitempty"`
	Layout    Layout   `json:"layout"`
}
type Definition struct {
	SchemaVersion int        `json:"schemaVersion"`
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Description   string     `json:"description,omitempty"`
	Variables     []Variable `json:"variables"`
	Widgets       []Widget   `json:"widgets"`
}
type Draft struct {
	ID            string     `json:"id"`
	Revision      int64      `json:"revision"`
	State         State      `json:"state"`
	SchemaVersion int        `json:"schemaVersion"`
	Definition    Definition `json:"definition"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	Owner         string     `json:"-"`
	Owned         bool       `json:"owned"`
}
type Page struct {
	Items      []Draft `json:"items"`
	NextCursor string  `json:"nextCursor,omitempty"`
}

var (
	ErrNotFound      = errors.New("dashboard draft not found")
	ErrConflict      = errors.New("dashboard revision conflict")
	ErrLimit         = errors.New("dashboard draft limit exceeded")
	ErrImmutable     = errors.New("approved dashboard is immutable")
	ErrInvalidState  = errors.New("invalid dashboard state transition")
	ErrInvalidCursor = errors.New("invalid dashboard cursor")
)

type Store interface {
	Ready(context.Context) error
	Create(context.Context, string, Definition) (Draft, error)
	List(context.Context, string, bool, string, int) (Page, error)
	Get(context.Context, string, string, bool) (Draft, error)
	Update(context.Context, string, string, int64, Definition) (Draft, error)
	Delete(context.Context, string, string, int64) error
	Submit(context.Context, string, string, int64) (Draft, error)
	Approve(context.Context, string, int64) (Draft, error)
}

func Canonical(def Definition) ([]byte, string, error) {
	b, err := canonicalBytes(def)
	if err != nil {
		return nil, "", err
	}
	return b, SHA256(b), nil
}

func canonicalBytes(def Definition) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(def); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func validateCanonicalSize(def Definition, limit int) error {
	b, err := canonicalBytes(def)
	if err != nil {
		return err
	}
	if len(b) > limit {
		return fmt.Errorf("canonical definition exceeds %d bytes", limit)
	}
	return nil
}
