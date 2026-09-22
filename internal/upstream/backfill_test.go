package upstream

import (
	"encoding/json"
	"testing"
)

// assistantRC 提取输出 messages 里各 assistant 消息的 reasoning_content（缺字段返回 ""+false）。
// 返回的切片与 messages 中 assistant 消息一一对应（非 assistant 消息跳过）。
func assistantRC(t *testing.T, out []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	var got []string
	msgs, _ := m["messages"].([]any)
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		if v, ok := msg["reasoning_content"].(string); ok {
			got = append(got, v)
		} else {
			got = append(got, "<absent>")
		}
	}
	return got
}

// TestBackfillReasoningContentDeepSeek DeepSeek 多轮一致性：历史 assistant 消息有
// reasoning 痕迹时，所有 assistant 消息必须带 reasoning_content（string）。
// 对齐官方 requiresReasoningContentOnAssistantMessages 行为。
func TestBackfillReasoningContentDeepSeek(t *testing.T) {
	cases := []struct {
		name string
		body string
		// 只断言「assistant 消息数」与「每条是否有 reasoning_content 字段」，
		// 值是 string 即可（复制或空串，由具体用例断言）。
		wantCount int
		wantVals  []string // 与 assistant 消息一一对应；空串表示任意 string
	}{
		{"assistant 带 reasoning 无 reasoning_content → 复制",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a","reasoning":"thought text"}]}`,
			1, []string{"thought text"}},
		{"assistant 带 reasoning_content 原样保留",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":"already there"}]}`,
			1, []string{"already there"}},
		{"混合会话全量补上：无 reasoning 的 assistant 补空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a1","reasoning":"t1"},
				{"role":"user","content":"u2"},
				{"role":"assistant","content":"a2"}]}`,
			2, []string{"t1", ""}},
		// 新契约下（门控 thinkingEnabled||hasTrace，issue #165）：L1 注入 enabled
		// 后零痕迹 assistant 也补空串——期望从 <absent> 改为 ""（存在 string）。
		{"assistant reasoning 为空串 → 视为零痕迹但 enabled 下补空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":""}]}`,
			1, []string{""}},
		{"多 assistant 都带 reasoning 全部复制",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a1","reasoning":"r1"},
				{"role":"assistant","content":"a2","reasoning":"r2"}]}`,
			2, []string{"r1", "r2"}},
		// 官方 "string"!=typeof 语义：非 string reasoning 无从复制，落补 "" 分支。
		{"assistant reasoning 非 string 值（数字）→ 补空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":123}]}`,
			1, []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantRC(t, out)
			if len(got) != c.wantCount {
				t.Fatalf("assistant 消息数 = %d want %d (out=%s)", len(got), c.wantCount, out)
			}
			for i, want := range c.wantVals {
				if want == "" {
					continue // 任意 string（补空串/复制都接受，但必须存在）
				}
				if got[i] != want {
					t.Errorf("assistant[%d].reasoning_content = %q want %q (out=%s)", i, got[i], want, out)
				}
			}
		})
	}
}

// TestBackfillReasoningContentNoTrace 会话无 reasoning 痕迹 + thinking disabled
// → 零改动：不白白给 assistant 消息加 reasoning_content 字段。
// 官方 ReasoningContentBackfillRule 门控 = thinkingEnabled || hasTrace；disabled
// 且无痕迹时两个半边都不亮 → 不补（issue #165 前该用例不分 disabled 与否一律不补，
// 现按新契约收紧为 disabled 形态——纯 text + enabled 补空串由
// TestBackfillZeroTraceThinkingEnabled 覆盖）。
func TestBackfillReasoningContentNoTrace(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"disabled + 纯 text assistant 不动",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"plain answer"}]}`},
		{"无 assistant 消息不动",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"}]}`},
		{"messages 缺失不动",
			`{"model":"deepseek-v4-flash"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			for _, rc := range assistantRC(t, out) {
				if rc != "<absent>" {
					t.Errorf("无 reasoning 痕迹却被加 reasoning_content=%q (out=%s)", rc, out)
				}
			}
		})
	}
}

// TestBackfillReasoningContentNonDeepSeek 非 deepseek 模型零改动：
// reasoning 字段保持原样，不新增 reasoning_content。
func TestBackfillReasoningContentNonDeepSeek(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":"a","reasoning":"thought"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	for _, rc := range assistantRC(t, out) {
		if rc != "<absent>" {
			t.Errorf("非 deepseek 不应 backfill, got reasoning_content=%q (out=%s)", rc, out)
		}
	}
}

// TestBackfillReasoningContentBothFields 同时带 reasoning 与 reasoning_content：
// 以 reasoning_content 为准（不覆盖），reasoning 字段保留（兼容）——对齐客户端 matches 规则。
func TestBackfillReasoningContentBothFields(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"assistant","content":"a","reasoning":"t","reasoning_content":"existing"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "existing" {
		t.Errorf("reasoning_content 应以已有值为准: got %v (out=%s)", got, out)
	}
	// 同时确认 reasoning 字段仍原样保留。
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if r, ok := first["reasoning"].(string); !ok || r != "t" {
		t.Errorf("reasoning 字段被改动: %v (out=%s)", first, out)
	}
}

// TestBackfillComposesWithInjectThinking backfill 与 injectThinking 组合语义：
// 显式 disabled 时 reasoning_effort 被删，但 backfill 的 hasTrace 半边照常生效
// （多轮一致性不因关思维链而丢）；disabled + 零痕迹则零改动（thinkingEnabled 半边不亮）。
func TestBackfillComposesWithInjectThinking(t *testing.T) {
	// R6：disabled + 有痕迹 → 照补（复制 reasoning）。
	body := `{"model":"DEEPSEEK-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"a","reasoning":"thought"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "thought" {
		t.Errorf("disabled 时 backfill 仍应生效: got %v (out=%s)", got, out)
	}
	if typ, present := getThinkingType(t, out); !present || typ != "disabled" {
		t.Errorf("thinking.type 应保留 disabled, got %q present=%v", typ, present)
	}
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := objFieldString(t, out, k); ok {
			t.Errorf("%s 应被删除（disabled 时）", k)
		}
	}

	// R2：disabled + 零痕迹 → 零改动（新契约四分支之一）。
	body = `{"model":"DEEPSEEK-v4-flash","thinking":{"type":"disabled"},"messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"plain"}]}`
	out = PrepareBodyOptWithEfforts([]byte(body), false, nil)
	for _, rc := range assistantRC(t, out) {
		if rc != "<absent>" {
			t.Errorf("disabled+零痕迹 不应 backfill, got reasoning_content=%q (out=%s)", rc, out)
		}
	}
}

// TestBackfillZeroTraceThinkingEnabled issue #165 复现锚（R1）：零痕迹多轮 deepseek，
// 经 L1 injectThinking 注入 enabled 后，thinkingEnabled 半边亮 → 每条 assistant
// 保证 reasoning_content 是 string（此处无 reasoning 可复制，全补空串）。
// 官方 ReasoningContentBackfillRule 的门控是 thinkingEnabled || hasTrace，
// 第三方客户端丢推理回传（零痕迹）形态下官方仍补，网关此前只移植了 hasTrace 半边。
func TestBackfillZeroTraceThinkingEnabled(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a1"},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":"a2"},
		{"role":"user","content":"u3"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	// 前置：出站确实是注入后的 enabled 形态（thinkingEnabled 判定读注入后请求体）。
	if typ, present := getThinkingType(t, out); !present || typ != "enabled" {
		t.Fatalf("thinking.type=%q present=%v want enabled (out=%s)", typ, present, out)
	}
	got := assistantRC(t, out)
	if len(got) != 2 {
		t.Fatalf("assistant 消息数 = %d want 2 (out=%s)", len(got), out)
	}
	for i, rc := range got {
		if rc != "" { // 既有 string（含空串）即满足；<absent>（键不存在）不满足
			t.Errorf("零痕迹 enabled 形态下 assistant[%d].reasoning_content = %q want 存在且为 \"\" (out=%s)", i, rc, out)
		}
	}
}

// TestBackfillNullAndNonStringNormalized issue #165 null/非 string 归一化（R3）：
// 官方跳过条件是 "string"!=typeof reasoning_content 才动手——null/数字会被旧代码
// 的 if _, ok（键存在即跳过）当「已有」跳过，新契约归一化为 ""。
// 注意 hasTrace 半边：reasoning_content 键存在本身即痕迹，门控必然亮。
func TestBackfillNullAndNonStringNormalized(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"reasoning_content:null 归一化为空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":null}]}`},
		{"reasoning_content:数字 归一化为空串",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":123}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantRC(t, out)
			if len(got) != 1 {
				t.Fatalf("assistant 消息数 = %d want 1 (out=%s)", len(got), out)
			}
			if got[0] != "" || got[0] == "<absent>" {
				t.Errorf("reasoning_content 应归一化为 \"\", got %q (out=%s)", got[0], out)
			}
		})
	}
}
