// Package secrets owns the plugin_config blob: AES-GCM at rest with a
// single host-wide key, JSON inside. The userID parameter is the seam
// where per-user auth lands later; today every caller passes "singleton"
// and the table's primary key is (user_id, plugin_id).
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Manager guards the AES-GCM key + the platform.plugin_config table. The
// key never leaves this struct; callers only see decoded JSON.
type Manager struct {
	pool *pgxpool.Pool
	key  []byte
}

// New decodes base64Key and verifies it's exactly 32 bytes (AES-256). We
// refuse 128/192-bit keys so the operator-facing knob has one shape.
func New(pool *pgxpool.Pool, base64Key string) (*Manager, error) {
	if base64Key == "" {
		return nil, errors.New("base64Key is required")
	}
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key must decode to 32 bytes, got %d", len(key))
	}
	return &Manager{pool: pool, key: key}, nil
}

// Get returns nil cfg + nil err when the row is missing -- callers treat
// that as "plugin has no saved config yet" and pass an empty Struct to
// Hello.config. A decrypt failure is a real error (likely key rotation).
func (m *Manager) Get(ctx context.Context, userID, pluginID string) (map[string]any, error) {
	if userID == "" {
		userID = "singleton"
	}
	var blob []byte
	err := m.pool.QueryRow(ctx, `
		SELECT config_blob
		FROM platform.plugin_config
		WHERE user_id = $1 AND plugin_id = $2
	`, userID, pluginID).Scan(&blob)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("select plugin_config: %w", err)
	}
	plain, err := m.decrypt(blob)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(plain, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return cfg, nil
}

// Set replaces the blob wholesale. We don't merge -- plugins that want
// patch semantics should call Get, mutate, then Set so the merge is
// visible at the call site.
func (m *Manager) Set(ctx context.Context, userID, pluginID string, cfg map[string]any) error {
	if userID == "" {
		userID = "singleton"
	}
	plain, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	blob, err := m.encrypt(plain)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	_, err = m.pool.Exec(ctx, `
		INSERT INTO platform.plugin_config (user_id, plugin_id, config_blob, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (user_id, plugin_id) DO UPDATE
		   SET config_blob = EXCLUDED.config_blob,
		       updated_at  = EXCLUDED.updated_at
	`, userID, pluginID, blob)
	if err != nil {
		return fmt.Errorf("upsert plugin_config: %w", err)
	}
	return nil
}

// Delete is idempotent -- the absence of the row is the goal state.
func (m *Manager) Delete(ctx context.Context, userID, pluginID string) error {
	if userID == "" {
		userID = "singleton"
	}
	_, err := m.pool.Exec(ctx, `
		DELETE FROM platform.plugin_config
		WHERE user_id = $1 AND plugin_id = $2
	`, userID, pluginID)
	if err != nil {
		return fmt.Errorf("delete plugin_config: %w", err)
	}
	return nil
}

// encrypt produces nonce || ciphertext-with-tag. We prefix the nonce
// instead of using AAD because the table only stores opaque bytes; the
// nonce has no semantic meaning to verify.
func (m *Manager) encrypt(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plain, nil)
	return append(nonce, ct...), nil
}

func (m *Manager) decrypt(blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(m.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, errors.New("blob shorter than nonce")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}
