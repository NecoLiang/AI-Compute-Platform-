package auth

import (
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID           int64     `db:"id" json:"id"`
	Phone        string    `db:"phone" json:"phone"`
	Email        string    `db:"email" json:"email"`
	PasswordHash string    `db:"password_hash" json:"-"`
	Status       string    `db:"status" json:"status"` // active/frozen
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

type Repository struct {
	db *sqlx.DB
}

func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) CreateUser(phone, email, passwordHash string) (int64, error) {
	tx, err := r.db.Beginx()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		"INSERT INTO users (phone, email, password_hash, status) VALUES (?, ?, ?, 'active')",
		phone, email, passwordHash,
	)
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return 0, ErrUserExists
		}
		return 0, err
	}
	userID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec("INSERT INTO user_roles (user_id, role) VALUES (?, 'buyer')", userID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return userID, nil
}

func (r *Repository) FindByPhone(phone string) (*User, error) {
	var u User
	err := r.db.Get(&u, "SELECT * FROM users WHERE phone = ?", phone)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *Repository) FindByID(id int64) (*User, error) {
	var u User
	err := r.db.Get(&u, "SELECT * FROM users WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *Repository) UpdatePassword(id int64, hash string) error {
	_, err := r.db.Exec("UPDATE users SET password_hash = ? WHERE id = ?", hash, id)
	return err
}

func (r *Repository) UpdateStatus(id int64, status string) error {
	_, err := r.db.Exec("UPDATE users SET status = ? WHERE id = ?", status, id)
	return err
}

func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
