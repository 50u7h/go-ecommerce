package models

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"log"
	"time"
)

const (
	ScopeAuthentication = "authentication"
)

// Token is the type for authentication tokens
type Token struct {
	PlainText string    `json:"token"`
	UserID    int64     `json:"-"`
	Hash      []byte    `json:"-"`
	Expiry    time.Time `json:"expiry"`
	Scope     string    `json:"-"`
}

// GenerateToken generates a token that lasts for ttl, and returns it
func GenerateToken(userID int, ttl time.Duration, scope string) (*Token, error) {
	token := &Token{
		UserID: int64(userID),
		Expiry: time.Now().Add(ttl),
		Scope:  scope,
	}

	randomBytes := make([]byte, 16)
	_, err := rand.Read(randomBytes)
	if err != nil {
		return nil, err
	}

	token.PlainText = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randomBytes)
	hash := sha256.Sum256(([]byte(token.PlainText)))
	token.Hash = hash[:]
	return token, nil
}

func (m *DBModel) InsertToken(token *Token, user User) error {

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()

	// delete existing tokens
	query := `delete from tokens where user_id = ?`
	_, err := m.DB.ExecContext(ctx, query, user.ID)
	if err != nil {
		return err
	}

	query = `
		insert into tokens
			(user_id, name, email, token_hash, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?)`

	_, err = m.DB.ExecContext(ctx, query,
		user.ID,
		user.LastName,
		user.Email,
		token.Hash,
		time.Now(),
		time.Now(),
	)
	if err != nil {
		return err
	}

	return nil
}

func (m *DBModel) GetUserForToken(token string) (*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()

	tokenHash := sha256.Sum256([]byte(token))
	var user User

	query := `
		select
			u.id, u.first_name, u.last_name, u.email
		from
			users u
			inner join tokens t on (u.id = t.user_id)
		where
			t.token_hash = ?
			and t.expiry > ?
	`
	err := m.DB.QueryRowContext(ctx, query, tokenHash[:] /*, time.Now()*/).Scan(
		&user.ID,
		&user.FirstName,
		&user.LastName,
		&user.Email,
	)

	if err != nil {
		log.Println(err)
		return nil, err
	}

	return &user, nil
}
