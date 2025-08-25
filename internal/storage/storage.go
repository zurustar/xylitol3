package storage

import (
	"database/sql"
	"fmt"

	_ "github.com/glebarez/go-sqlite" // 純粋なGoのSQLiteドライバ
)

// User は、SIP認証用のユーザーアカウントを表します。
type User struct {
	ID       int64
	Username string
	Password string // これはRFC 2617/2069に従ってHA1ハッシュを保存します
}

// Storage は、アプリケーションのデータベース操作を処理します。
type Storage struct {
	db *sql.DB
}

// NewStorage は、新しいストレージサービスを初期化します。
// SQLiteデータベースへの接続を開き、必要なテーブルが存在することを確認します。
func NewStorage(dataSourceName string) (*Storage, error) {
	db, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("could not open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("could not connect to database: %w", err)
	}

	if err := createTables(db); err != nil {
		return nil, fmt.Errorf("could not create tables: %w", err)
	}

	return &Storage{db: db}, nil
}

// createTables は、データベーススキーマを設定します。
func createTables(db *sql.DB) error {
	const usersTable = `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		password TEXT NOT NULL
	);
	`
	if _, err := db.Exec(usersTable); err != nil {
		return fmt.Errorf("could not create users table: %w", err)
	}
	return nil
}

// Close は、データベース接続を閉じます。
func (s *Storage) Close() error {
	return s.db.Close()
}

// AddUser は、新しいユーザーをデータベースに追加します。
func (s *Storage) AddUser(user *User) error {
	stmt, err := s.db.Prepare("INSERT INTO users(username, password) VALUES(?, ?)")
	if err != nil {
		return fmt.Errorf("could not prepare statement for adding user: %w", err)
	}
	defer stmt.Close()

	res, err := stmt.Exec(user.Username, user.Password)
	if err != nil {
		return fmt.Errorf("could not execute statement for adding user: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("could not get last insert ID: %w", err)
	}
	user.ID = id
	return nil
}

// GetUserByUsername は、ユーザー名でデータベースからユーザーを取得します。
func (s *Storage) GetUserByUsername(username string) (*User, error) {
	stmt, err := s.db.Prepare("SELECT id, username, password FROM users WHERE username = ?")
	if err != nil {
		return nil, fmt.Errorf("could not prepare statement for getting user: %w", err)
	}
	defer stmt.Close()

	user := &User{}
	err = stmt.QueryRow(username).Scan(&user.ID, &user.Username, &user.Password)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // ユーザーが見つからないことはアプリケーションエラーではありません
		}
		return nil, fmt.Errorf("could not query user: %w", err)
	}
	return user, nil
}

// GetAllUsers は、データベースからすべてのユーザーを取得します。
func (s *Storage) GetAllUsers() ([]*User, error) {
	rows, err := s.db.Query("SELECT id, username, password FROM users ORDER BY username")
	if err != nil {
		return nil, fmt.Errorf("could not query all users: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		user := &User{}
		if err := rows.Scan(&user.ID, &user.Username, &user.Password); err != nil {
			return nil, fmt.Errorf("could not scan user row: %w", err)
		}
		users = append(users, user)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error during rows iteration: %w", err)
	}

	return users, nil
}
