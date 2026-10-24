package admin

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidInput = errors.New("invalid input")
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrLastAdmin    = errors.New("last active admin")
)

type Repository struct{ db *pgxpool.Pool }

func NewRepository(db *pgxpool.Pool) *Repository { return &Repository{db: db} }

type User struct {
	ID       uuid.UUID `json:"id"`
	Username string    `json:"username"`
	IsAdmin  bool      `json:"is_admin"`
	IsActive bool      `json:"is_active"`
}

type Group struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type Grant struct {
	DatabaseID uuid.UUID `json:"database_id"`
	Access     string    `json:"access"`
}

func validUsername(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= 3 && len(value) <= 64 && value == strings.TrimSpace(value)
}

func validGroupName(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 100 && value == strings.TrimSpace(value)
}

func validPassword(value string) bool {
	if len(value) < 12 || len(value) > 1024 {
		return false
	}
	return len(bcryptHash(value)) > 0
}

func bcryptHash(value string) []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte(value), bcrypt.DefaultCost)
	if err != nil {
		return nil
	}
	return hash
}

func (r *Repository) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := r.db.Query(ctx, `SELECT id,username,is_admin,is_active FROM app.users ORDER BY username,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]User, 0)
	for rows.Next() {
		var item User
		if err := rows.Scan(&item.ID, &item.Username, &item.IsAdmin, &item.IsActive); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *Repository) CreateUser(ctx context.Context, username, password string, isAdmin bool) (User, error) {
	if !validUsername(username) || !validPassword(password) {
		return User{}, ErrInvalidInput
	}
	hash := bcryptHash(password)
	var user User
	err := r.db.QueryRow(ctx, `INSERT INTO app.users(id,username,password_hash,is_admin,is_active)
		VALUES($1,$2,$3,$4,true) RETURNING id,username,is_admin,is_active`,
		uuid.New(), username, hash, isAdmin).Scan(&user.ID, &user.Username, &user.IsAdmin, &user.IsActive)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return User{}, ErrConflict
		}
		return User{}, err
	}
	return user, nil
}

func (r *Repository) UpdateUser(ctx context.Context, id uuid.UUID, username, password *string, isAdmin, isActive *bool) (User, error) {
	if id == uuid.Nil {
		return User{}, ErrInvalidInput
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx)

	var current User
	if err := tx.QueryRow(ctx, `SELECT id,username,is_admin,is_active FROM app.users WHERE id=$1 FOR UPDATE`, id).
		Scan(&current.ID, &current.Username, &current.IsAdmin, &current.IsActive); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	if username != nil && !validUsername(*username) {
		return User{}, ErrInvalidInput
	}
	if password != nil && !validPassword(*password) {
		return User{}, ErrInvalidInput
	}
	nextAdmin, nextActive := current.IsAdmin, current.IsActive
	if isAdmin != nil {
		nextAdmin = *isAdmin
	}
	if isActive != nil {
		nextActive = *isActive
	}
	if current.IsAdmin && current.IsActive && (!nextAdmin || !nextActive) {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM app.users WHERE is_admin AND is_active`).Scan(&count); err != nil {
			return User{}, err
		}
		if count <= 1 {
			return User{}, ErrLastAdmin
		}
	}
	set := []string{"is_admin=$2", "is_active=$3", "updated_at=clock_timestamp()", "auth_version=auth_version+1"}
	args := []any{id, nextAdmin, nextActive}
	index := 4
	if username != nil {
		set = append(set, "username=$"+itoa(index))
		args = append(args, *username)
		index++
	}
	if password != nil {
		set = append(set, "password_hash=$"+itoa(index))
		args = append(args, bcryptHash(*password))
		index++
	}
	query := "UPDATE app.users SET " + strings.Join(set, ",") + " WHERE id=$1 RETURNING id,username,is_admin,is_active"
	if err := tx.QueryRow(ctx, query, args...).Scan(&current.ID, &current.Username, &current.IsAdmin, &current.IsActive); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return User{}, ErrConflict
		}
		return User{}, err
	}
	if err := revokeUsers(ctx, tx, id); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return current, nil
}

func (r *Repository) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := r.db.Query(ctx, `SELECT id,name FROM app.groups ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Group, 0)
	for rows.Next() {
		var item Group
		if err := rows.Scan(&item.ID, &item.Name); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *Repository) CreateGroup(ctx context.Context, name string) (Group, error) {
	if !validGroupName(name) {
		return Group{}, ErrInvalidInput
	}
	var item Group
	err := r.db.QueryRow(ctx, `INSERT INTO app.groups(id,name) VALUES($1,$2) RETURNING id,name`, uuid.New(), name).
		Scan(&item.ID, &item.Name)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return Group{}, ErrConflict
		}
		return Group{}, err
	}
	return item, nil
}

func (r *Repository) UpdateGroup(ctx context.Context, id uuid.UUID, name string) (Group, error) {
	if id == uuid.Nil || !validGroupName(name) {
		return Group{}, ErrInvalidInput
	}
	var item Group
	err := r.db.QueryRow(ctx, `UPDATE app.groups SET name=$2,updated_at=clock_timestamp() WHERE id=$1 RETURNING id,name`, id, name).
		Scan(&item.ID, &item.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return Group{}, ErrConflict
		}
		return Group{}, err
	}
	return item, nil
}

func (r *Repository) DeleteGroup(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return ErrInvalidInput
	}
	result, err := r.db.Exec(ctx, `DELETE FROM app.groups WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) Members(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx, `SELECT user_id FROM app.group_memberships WHERE group_id=$1 ORDER BY user_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]uuid.UUID, 0)
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		result = append(result, userID)
	}
	return result, rows.Err()
}

func (r *Repository) ReplaceMembers(ctx context.Context, id uuid.UUID, userIDs []uuid.UUID) ([]uuid.UUID, error) {
	return r.replaceSet(ctx, id, userIDs, nil)
}

func (r *Repository) Grants(ctx context.Context, id uuid.UUID) ([]Grant, error) {
	rows, err := r.db.Query(ctx, `SELECT database_id,access FROM app.group_database_grants WHERE group_id=$1 ORDER BY database_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Grant, 0)
	for rows.Next() {
		var item Grant
		if err := rows.Scan(&item.DatabaseID, &item.Access); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *Repository) ReplaceGrants(ctx context.Context, id uuid.UUID, grants []Grant) ([]Grant, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM app.groups WHERE id=$1 FOR UPDATE`, id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app.group_database_grants WHERE group_id=$1`, id); err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	for _, grant := range grants {
		if grant.DatabaseID == uuid.Nil || (grant.Access != "view" && grant.Access != "edit") || seen[grant.DatabaseID] {
			return nil, ErrInvalidInput
		}
		seen[grant.DatabaseID] = true
		if _, err := tx.Exec(ctx, `INSERT INTO app.group_database_grants(group_id,database_id,access) VALUES($1,$2,$3)`, id, grant.DatabaseID, grant.Access); err != nil {
			if strings.Contains(err.Error(), "foreign key") {
				return nil, ErrInvalidInput
			}
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Grants(ctx, id)
}

func (r *Repository) replaceSet(ctx context.Context, groupID uuid.UUID, userIDs []uuid.UUID, _ any) ([]uuid.UUID, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM app.groups WHERE id=$1 FOR UPDATE`, groupID); err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range userIDs {
		if id == uuid.Nil || seen[id] {
			return nil, ErrInvalidInput
		}
		seen[id] = true
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM app.users WHERE id=$1)`, id).Scan(&exists); err != nil || !exists {
			return nil, ErrInvalidInput
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app.group_memberships WHERE group_id=$1`, groupID); err != nil {
		return nil, err
	}
	for _, id := range userIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO app.group_memberships(group_id,user_id) VALUES($1,$2)`, groupID, id); err != nil {
			return nil, err
		}
		if err := revokeUsers(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Members(ctx, groupID)
}

func revokeUsers(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE app.session_families SET revoked_at=$2 WHERE user_id=$1 AND revoked_at IS NULL`, userID, time.Now().UTC())
	return err
}

func itoa(value int) string {
	if value == 4 {
		return "4"
	}
	return "5"
}
