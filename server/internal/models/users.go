package models

import (
	"database/sql"
	"time"

	"backup-server/internal/idgen"
)

func CreateUser(db *sql.DB, username, passwordHash string) (*User, error) {
	u := &User{
		ID:           idgen.New(),
		Username:     username,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	_, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.PasswordHash, toDB(u.CreatedAt), toDB(u.UpdatedAt),
	)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func GetUserByUsername(db *sql.DB, username string) (*User, error) {
	row := db.QueryRow(`SELECT id, username, password_hash, created_at, updated_at FROM users WHERE username = ?`, username)
	return scanUser(row)
}

func GetUserByID(db *sql.DB, id string) (*User, error) {
	row := db.QueryRow(`SELECT id, username, password_hash, created_at, updated_at FROM users WHERE id = ?`, id)
	return scanUser(row)
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	var created, updated string
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &created, &updated); err != nil {
		return nil, err
	}
	u.CreatedAt = fromDB(created)
	u.UpdatedAt = fromDB(updated)
	return &u, nil
}

func UpdateUserPassword(db *sql.DB, id, passwordHash string) error {
	_, err := db.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, passwordHash, toDB(time.Now()), id)
	return err
}

func UpdateUsername(db *sql.DB, id, username string) error {
	_, err := db.Exec(`UPDATE users SET username = ?, updated_at = ? WHERE id = ?`, username, toDB(time.Now()), id)
	return err
}

func CountUsers(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}
