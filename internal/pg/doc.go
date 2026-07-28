// Package pg — PG 连接与事务封装。
//
// Task 0.3 结论决定 InTx 签名:若可共享连接,签名为
// func(context.Context, *pgx.Conn) error;否则退化为独立事务 + 时间戳。
package pg
