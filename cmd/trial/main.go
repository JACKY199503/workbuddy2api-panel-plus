// trial 一次性批量领取 global trial 加油包：遍历 auths 下全部账号，
// 仅对 global 账号执行 /billing/ide/trial（CN 无此端点，明确提示不适用）。
//
// 用法：
//
//	# 本地：在项目根目录（需 config.json + auths/ + data/）直接 run
//	go run ./cmd/trial
//
//	# 容器内：先 cp 进去再 exec
//	docker cp trial workbuddy2api:/tmp/trial
//	docker exec -w /app workbuddy2api /tmp/trial
//
// 结果逐账号输出到 stdout：
//
//	uid | nick | status | detail
//	----+------+--------+-------
//	... | GLOBAL | OK | trial granted
//	... | GLOBAL | ALREADY | 已领取过（幂等，不算失败）
//	... | CN     | N/A   | not applicable
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// classifyTrial 归一化 ClaimTrial 结果（纯函数，供 main 循环与测试直接断言）：
// err → FAIL；claimed → OK；否则（幂等码已领）→ ALREADY。
func classifyTrial(claimed bool, err error) (trialStatus, string) {
	switch {
	case err != nil:
		return trialFailed, err.Error()
	case claimed:
		return trialOK, "trial granted"
	default:
		return trialAlready, "already claimed (idempotent)"
	}
}

// trialStatus 单账号 trial 领取结果状态。
type trialStatus string

const (
	trialOK      trialStatus = "OK"
	trialAlready trialStatus = "ALREADY"
	trialNotApp  trialStatus = "N/A" // CN 账号不适用
	trialFailed  trialStatus = "FAIL"
)

type trialRow struct {
	uid    string
	nick   string
	status trialStatus
	detail string
}

func main() {
	authDir := "auths"
	if len(os.Args) > 1 {
		authDir = os.Args[1]
	}
	files, err := filepath.Glob(filepath.Join(authDir, "workbuddy-*.json"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", authDir)
		os.Exit(1)
	}
	sort.Strings(files)

	up := upstream.New()
	// trial 是 global 专属端点：必须开启 global realm 路由，否则 upstream.New() 的
	// GlobalEnabled 零值 false 会把请求路由到 CN base（codebuddy.cn）而必然失败。
	up.GlobalEnabled = true
	var rows []trialRow
	for _, f := range files {
		r := trialRow{uid: filepath.Base(f)}
		raw, err := os.ReadFile(f)
		if err != nil {
			r.status, r.detail = trialFailed, "load: "+err.Error()
			rows = append(rows, r)
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			r.status, r.detail = trialFailed, "parse: "+err.Error()
			rows = append(rows, r)
			continue
		}
		a.FilePath = f
		r.uid, r.nick = a.UID, a.Nickname

		// 仅 global 账号适用：CN 明确提示不适用，不发任何请求。
		if !a.IsGlobal() {
			r.status, r.detail = trialNotApp, "CN account not applicable"
			rows = append(rows, r)
			continue
		}

		r.status, r.detail = classifyTrial(up.ClaimTrial(a))
		rows = append(rows, r)
	}

	var okN, alreadyN, notAppN, failN int
	fmt.Printf("uid                                  | nick        | status  | detail\n")
	fmt.Printf("-------------------------------------+-------------+---------+------------------------------\n")
	for _, r := range rows {
		fmt.Printf("%-36s | %-11s | %-7s | %s\n",
			trunc(r.uid, 36), trunc(r.nick, 11), r.status, r.detail)
		switch r.status {
		case trialOK:
			okN++
		case trialAlready:
			alreadyN++
		case trialNotApp:
			notAppN++
		default:
			failN++
		}
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d na=%d fail=%d\n",
		len(rows), okN, alreadyN, notAppN, failN)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
