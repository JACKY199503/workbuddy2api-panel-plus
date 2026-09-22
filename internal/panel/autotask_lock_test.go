package panel

import (
	"testing"
)

// TestTaskAccountLockSameAccountExclusive 同一账号的任务锁互斥：第二次 tryLock 必须失败，
// 解锁后可再次获取。这是「重复点一键完成不并发重跑」的核心保障。
func TestTaskAccountLockSameAccountExclusive(t *testing.T) {
	p := &Panel{}
	uid := "u1"

	if !p.tryLockAccount(uid) {
		t.Fatal("首次加锁应成功")
	}
	if p.tryLockAccount(uid) {
		t.Fatal("同账号第二次加锁应失败（互斥）")
	}
	p.unlockAccount(uid)

	if !p.tryLockAccount(uid) {
		t.Fatal("解锁后应可再次加锁")
	}
	p.unlockAccount(uid)
}

// TestTaskAccountLockDifferentAccountsIndependent 不同账号的锁互不影响（并行照旧）。
func TestTaskAccountLockDifferentAccountsIndependent(t *testing.T) {
	p := &Panel{}
	if !p.tryLockAccount("u1") {
		t.Fatal("u1 加锁应成功")
	}
	if !p.tryLockAccount("u2") {
		t.Fatal("u2 加锁应成功（不同账号不互斥）")
	}
	p.unlockAccount("u2")
	p.unlockAccount("u1")
}

// TestTaskAccountLockCrossEntryShared 单任务 auto 与全量 auto_all 共用同一把账号锁
// （在 handler 层都走 tryLockAccount，这里验证锁命名空间一致）。
func TestTaskAccountLockCrossEntryShared(t *testing.T) {
	p := &Panel{}
	if !p.tryLockAccount("u1") {
		t.Fatal("u1 加锁应成功")
	}
	// 模拟全量入口对同一 uid 加锁——必须被挡（否则两入口可并发）。
	if p.tryLockAccount("u1") {
		t.Fatal("同 uid 跨入口加锁应失败（共用锁）")
	}
	p.unlockAccount("u1")
}
