// Package repo 仓储层公共部分:Querier 抽象、事务辅助、错误映射。
//
// Querier 同时被 *pgxpool.Pool 与 pgx.Tx 满足,因此同一份 SQL 既可在池上执行,
// 也可在事务内执行,避免"忘记传 tx 导致写不进事务"这类事故。
package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier 是 pgxpool.Pool 与 pgx.Tx 的公共子集。
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repos 聚合各仓储,便于在 handler 中注入一个对象。
type Repos struct {
	User   UserRepo
	Space  SpaceRepo
	File   FileRepo
	Upload UploadRepo
	Dept   DeptRepo
}

// New 构造聚合仓储。
func New() *Repos {
	return &Repos{}
}

// IsUniqueViolation 判断是否唯一约束冲突(映射为 409)。
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsForeignKeyViolation 判断是否外键冲突(通常意味着参数指向不存在的对象)。
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// IsCheckViolation 判断是否 CHECK 约束冲突(多为业务不变量被破坏,属代码缺陷)。
func IsCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}
