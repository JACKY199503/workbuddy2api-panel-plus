#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
t_compat.py — 协议兼容层（/v1/responses、/v1/messages）全模型全接口回归。

覆盖：
  正向  每个模型 × 3 个接口 × (非流式 / 流式) = 6 条用例
  反向  鉴权缺失/错误、非法 JSON、缺字段、空数组、未知模型、非法 role、方法不允许

判定口径：
  PASS     结构正确且拿到正文
  WARN     200 但正文为空（上游空回复，非协议问题）
  UPSTREAM 上游返回错误，但网关按目标协议形态正确封装（协议正确，上游不可用）
  FAIL     协议形态不对 / 状态码不对 / 结构缺失

用法：
  python3 t_compat.py                 # 全量
  python3 t_compat.py --models cn:hy3 # 只测指定模型（逗号分隔）
  python3 t_compat.py --no-stream     # 跳过流式
"""
import json
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = "http://127.0.0.1:7863"
CFG = "/opt/wb2api/config/config.json"
PROMPT = "请用一句话介绍你自己。"  # 必产生非空正文；极短提示易被 reasoning 模型吃成空回复
MAXTOK = 256  # reasoning 模型思考 token 会占用预算，给足避免 length 截断
TIMEOUT = 120
RETRY_EMPTY = 1  # 空正文重试次数（上游偶发空回复，非协议问题）
WORKERS = 2      # 并发度：过高会触发上游限流空回复，压低以区分偶发与真实失败


def apikey():
    with open(CFG, "r", encoding="utf-8") as f:
        return json.load(f)["api_key"]


KEY = apikey()


def http(method, path, body=None, headers=None, stream=False, timeout=TIMEOUT):
    """返回 (status, body_text, content_type)；body 为 None 表示无 body。"""
    data = None
    if body is not None:
        data = body.encode("utf-8") if isinstance(body, str) else json.dumps(body).encode("utf-8")
    hdr = {"Authorization": "Bearer " + KEY, "Content-Type": "application/json"}
    if headers:
        hdr.update(headers)
    if headers and headers.get("__noauth__"):
        hdr.pop("Authorization", None)
        hdr.pop("__noauth__", None)
    req = urllib.request.Request(BASE + path, data=data, headers=hdr, method=method)
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace"), ""
    except Exception as e:  # 传输层错误
        return 0, "TRANSPORT: %s" % e, ""
    ct = resp.headers.get("Content-Type", "")
    raw = resp.read().decode("utf-8", "replace")
    return resp.status, raw, ct


def http_stream(path, body, timeout=TIMEOUT):
    """返回 (status, events, ct)：events = [(event, data_str)]，data 为 [DONE] 时 event 为空。"""
    data = json.dumps(body).encode("utf-8")
    hdr = {"Authorization": "Bearer " + KEY, "Content-Type": "application/json"}
    req = urllib.request.Request(BASE + path, data=data, headers=hdr, method="POST")
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as e:
        return e.code, [], e.read().decode("utf-8", "replace")
    except Exception as e:
        return 0, [], "TRANSPORT: %s" % e
    ct = resp.headers.get("Content-Type", "")
    events = []
    cur_event = ""
    try:
        for line in resp:
            s = line.decode("utf-8", "replace").rstrip("\r\n")
            if s.startswith("event: "):
                cur_event = s[7:]
            elif s.startswith("data: "):
                events.append((cur_event, s[6:]))
                cur_event = ""
    except Exception as e:
        events.append(("", "READERR: %s" % e))
    return resp.status, events, ct


# ── 判定 ────────────────────────────────────────────────────────────

def verdict_chat(status, raw):
    if status != 200:
        return upstream_or_fail(status, raw, "error")
    try:
        d = json.loads(raw)
    except Exception:
        return "FAIL", "bad json"
    ch = d.get("choices") or []
    if not ch:
        return "FAIL", "no choices"
    txt = ((ch[0].get("message") or {}).get("content") or "")
    if txt.strip():
        return "PASS", "%d chars" % len(txt)
    return "WARN", "empty content"


def verdict_responses(status, raw):
    if status != 200:
        return upstream_or_fail(status, raw, "error")
    try:
        d = json.loads(raw)
    except Exception:
        return "FAIL", "bad json"
    if d.get("object") != "response":
        return "FAIL", "object!=response"
    out = d.get("output") or []
    if not out or not (out[0].get("content") or []):
        return "FAIL", "no output content"
    txt = out[0]["content"][0].get("text") or ""
    for k in ("id", "created_at", "status", "model", "usage"):
        if k not in d:
            return "FAIL", "missing " + k
    if txt.strip():
        return "PASS", "%d chars" % len(txt)
    return "WARN", "empty text"


def verdict_messages(status, raw):
    if status != 200:
        return upstream_or_fail(status, raw, "error")
    try:
        d = json.loads(raw)
    except Exception:
        return "FAIL", "bad json"
    if d.get("type") != "message":
        return "FAIL", "type!=message"
    c = d.get("content") or []
    if not c:
        return "FAIL", "no content"
    txt = c[0].get("text") or ""
    for k in ("id", "role", "model", "stop_reason", "usage"):
        if k not in d:
            return "FAIL", "missing " + k
    if txt.strip():
        return "PASS", "%d chars" % len(txt)
    return "WARN", "empty text"


def upstream_or_fail(status, raw, errkey):
    """非 200：协议错误形态正确 → UPSTREAM；形态不对 → FAIL。"""
    try:
        d = json.loads(raw)
    except Exception:
        return "FAIL", "http %s non-json" % status
    if errkey == "error":  # OpenAI/Responses: {"error":{...}}; Anthropic: {"type":"error","error":{...}}
        if isinstance(d.get("error"), dict) and d["error"].get("message"):
            return "UPSTREAM", "http %s %s" % (status, d["error"].get("code") or d["error"].get("type") or "")
        if d.get("type") == "error" and isinstance(d.get("error"), dict):
            return "UPSTREAM", "http %s %s" % (status, d["error"].get("type") or "")
    return "FAIL", "http %s wrong error shape" % status


def verdict_chat_stream(status, events, ct):
    if status != 200:
        return "FAIL", "http %s" % status
    if "text/event-stream" not in ct:
        return "FAIL", "ct=%s" % ct
    txt = ""
    done = False
    for _, d in events:
        if d == "[DONE]":
            done = True
            continue
        try:
            o = json.loads(d)
        except Exception:
            continue
        if "error" in o:
            return "UPSTREAM", "stream error frame"
        for c in o.get("choices") or []:
            t = ((c.get("delta") or {}).get("content") or "")
            txt += t
    if not done:
        return "FAIL", "no [DONE]"
    if txt.strip():
        return "PASS", "%d chars" % len(txt)
    return "WARN", "empty delta"


def verdict_responses_stream(status, events, ct):
    if status != 200:
        return "FAIL", "http %s" % status
    if "text/event-stream" not in ct:
        return "FAIL", "ct=%s" % ct
    kinds = [e for e, _ in events]
    txt = ""
    status = ""
    finish = ""
    for e, d in events:
        if d == "[DONE]":
            continue
        try:
            o = json.loads(d)
        except Exception:
            continue
        if o.get("type") == "error":
            return "UPSTREAM", "stream error event"
        if o.get("type") == "response.output_text.delta":
            txt += o.get("delta") or ""
        if o.get("type") == "response.completed":
            status = ((o.get("response") or {}).get("status") or "")
        if o.get("type") == "message_delta":
            finish = ((o.get("delta") or {}).get("stop_reason") or "")
    for need in ("response.created", "response.completed"):
        if need not in kinds:
            return "FAIL", "missing " + need
    if txt.strip():
        return "PASS", "%d chars" % len(txt)
    if status == "incomplete" or finish == "max_tokens":
        return "WARN", "empty text (reasoning ate budget)"
    return "WARN", "empty delta"


def verdict_messages_stream(status, events, ct):
    if status != 200:
        return "FAIL", "http %s" % status
    if "text/event-stream" not in ct:
        return "FAIL", "ct=%s" % ct
    kinds = [e for e, _ in events]
    txt = ""
    stop = ""
    for e, d in events:
        try:
            o = json.loads(d)
        except Exception:
            continue
        if o.get("type") == "error":
            return "UPSTREAM", "stream error event"
        if o.get("type") == "content_block_delta":
            txt += ((o.get("delta") or {}).get("text") or "")
        if o.get("type") == "message_delta":
            stop = ((o.get("delta") or {}).get("stop_reason") or "")
    for need in ("message_start", "message_stop"):
        if need not in kinds:
            return "FAIL", "missing " + need
    if txt.strip():
        return "PASS", "%d chars" % len(txt)
    # 无正文：reasoning 模型把 max_tokens 预算全烧在思考上（上游行为，非协议问题）。
    if stop == "max_tokens":
        return "WARN", "empty text (reasoning ate budget)"
    if "content_block_delta" not in kinds:
        return "FAIL", "missing content_block_delta"
    return "WARN", "empty delta"


# ── 用例 ────────────────────────────────────────────────────────────

def case_model(m, do_stream=True):
    """单模型 6 条用例；结果为 WARN（空正文）时重试 RETRY_EMPTY 次取最好结果。"""
    out = []
    for attempt in range(RETRY_EMPTY + 1):
        out = []
        # 1. chat 非流式
        st, raw, _ = http("POST", "/v1/chat/completions", {"model": m, "messages": [{"role": "user", "content": PROMPT}], "max_tokens": MAXTOK})
        out.append(("chat", "non-stream", m) + verdict_chat(st, raw))
        # 2. responses 非流式
        st, raw, _ = http("POST", "/v1/responses", {"model": m, "input": PROMPT, "max_output_tokens": MAXTOK})
        out.append(("responses", "non-stream", m) + verdict_responses(st, raw))
        # 3. messages 非流式
        st, raw, _ = http("POST", "/v1/messages", {"model": m, "messages": [{"role": "user", "content": PROMPT}], "max_tokens": MAXTOK})
        out.append(("messages", "non-stream", m) + verdict_messages(st, raw))
        if do_stream:
            st, ev, ct = http_stream("/v1/chat/completions", {"model": m, "messages": [{"role": "user", "content": PROMPT}], "max_tokens": MAXTOK, "stream": True})
            out.append(("chat", "stream", m) + verdict_chat_stream(st, ev, ct))
            st, ev, ct = http_stream("/v1/responses", {"model": m, "input": PROMPT, "max_output_tokens": MAXTOK, "stream": True})
            out.append(("responses", "stream", m) + verdict_responses_stream(st, ev, ct))
            st, ev, ct = http_stream("/v1/messages", {"model": m, "messages": [{"role": "user", "content": PROMPT}], "max_tokens": MAXTOK, "stream": True})
            out.append(("messages", "stream", m) + verdict_messages_stream(st, ev, ct))
        if not any(r[3] == "WARN" for r in out):
            break
        if attempt < RETRY_EMPTY:
            time.sleep(2)
    return out


def case_negative():
    out = []
    M = "cn:hy3"

    def add(name, got, expect_shape, expect_status):
        st, raw, _ = got
        if st != expect_status:
            out.append((name, "FAIL", "http %s want %s" % (st, expect_status)))
            return
        try:
            d = json.loads(raw)
        except Exception:
            out.append((name, "FAIL", "non-json"))
            return
        err = d.get("error")
        ok = isinstance(err, dict) and bool(err.get("message"))
        if expect_shape == "anthropic":
            ok = ok and d.get("type") == "error"
        out.append((name, "PASS" if ok else "FAIL", "http %s %s" % (st, (err or {}).get("code") or (err or {}).get("type") or "")))

    # 鉴权
    add("no-auth chat", http("POST", "/v1/chat/completions", {"model": M, "messages": [{"role": "user", "content": "x"}]}, headers={"__noauth__": "1"}), "openai", 401)
    add("no-auth responses", http("POST", "/v1/responses", {"model": M, "input": "x"}, headers={"__noauth__": "1"}), "openai", 401)
    add("no-auth messages", http("POST", "/v1/messages", {"model": M, "messages": [{"role": "user", "content": "x"}], "max_tokens": 16}, headers={"__noauth__": "1"}), "anthropic", 401)
    add("bad-key messages", http("POST", "/v1/messages", {"model": M, "messages": [{"role": "user", "content": "x"}], "max_tokens": 16}, headers={"Authorization": "Bearer wrong"}), "anthropic", 401)

    # 非法 JSON：chat 无本地预校验，畸形 body 直达上游 → 期望非 2xx（上游给）
    st, raw, _ = http("POST", "/v1/chat/completions", "{not json")
    out.append(("bad-json chat", "PASS" if st >= 400 else "FAIL", "http %s" % st))

    add("bad-json responses", http("POST", "/v1/responses", "{not json"), "openai", 400)
    add("bad-json messages", http("POST", "/v1/messages", "{not json"), "anthropic", 400)

    # 缺字段
    add("no-model responses", http("POST", "/v1/responses", {"input": "x"}), "openai", 400)
    add("no-model messages", http("POST", "/v1/messages", {"messages": [{"role": "user", "content": "x"}], "max_tokens": 16}), "anthropic", 400)
    add("no-input responses", http("POST", "/v1/responses", {"model": M}), "openai", 400)
    add("empty-input responses", http("POST", "/v1/responses", {"model": M, "input": ""}), "openai", 400)
    add("empty-array-input responses", http("POST", "/v1/responses", {"model": M, "input": []}), "openai", 400)
    add("no-messages messages", http("POST", "/v1/messages", {"model": M, "max_tokens": 16}), "anthropic", 400)
    add("empty-messages messages", http("POST", "/v1/messages", {"model": M, "messages": [], "max_tokens": 16}), "anthropic", 400)
    add("bad-role messages", http("POST", "/v1/messages", {"model": M, "messages": [{"role": "alien", "content": "x"}], "max_tokens": 16}), "anthropic", 400)
    add("bad-input-type responses", http("POST", "/v1/responses", {"model": M, "input": {"a": 1}}), "openai", 400)

    # 方法不允许（GET 到 POST-only 端点）
    for p in ("/v1/chat/completions", "/v1/responses", "/v1/messages"):
        st, _, _ = http("GET", p)
        out.append(("GET " + p, "PASS" if st in (404, 405) else "FAIL", "http %s" % st))

    # 未知模型（期望非 2xx 且按协议形态封装）
    st, raw, _ = http("POST", "/v1/responses", {"model": "cn:no-such-model-zzz", "input": "x"})
    v, det = upstream_or_fail(st, raw, "error")
    out.append(("unknown-model responses", "PASS" if st != 200 and v == "UPSTREAM" else "FAIL", det))
    st, raw, _ = http("POST", "/v1/messages", {"model": "cn:no-such-model-zzz", "messages": [{"role": "user", "content": "x"}], "max_tokens": 16})
    v, det = upstream_or_fail(st, raw, "error")
    out.append(("unknown-model messages", "PASS" if st != 200 and v == "UPSTREAM" else "FAIL", det))
    return out


def main():
    args = sys.argv[1:]
    do_stream = "--no-stream" not in args
    models = None
    for a in args:
        if a.startswith("--models="):
            models = [x for x in a[len("--models="):].split(",") if x]
    if models is None:
        st, raw, _ = http("GET", "/v1/models")
        models = [m["id"] for m in json.loads(raw).get("data", [])]
    print("models(%d): %s" % (len(models), ",".join(models)))
    print("stream: %s" % do_stream)

    rows = []
    t0 = time.time()
    with ThreadPoolExecutor(max_workers=WORKERS) as ex:
        for res in ex.map(lambda m: case_model(m, do_stream), models):
            rows.extend(res)
    rows.extend(case_negative())

    # 汇总
    stat = {}
    for r in rows:
        stat[r[3] if len(r) > 3 else r[1]] = stat.get(r[3] if len(r) > 3 else r[1], 0) + 1

    print("\n=== 明细 ===")
    for r in rows:
        if len(r) == 5:
            print("%-10s %-11s %-22s %-8s %s" % (r[0], r[1], r[2], r[3], r[4]))
        else:
            print("%-43s %-8s %s" % (r[0], r[1], r[2]))
    print("\n=== 汇总(%d) ===" % len(rows))
    for k in sorted(stat):
        print("%-10s %d" % (k, stat[k]))
    print("elapsed %.1fs" % (time.time() - t0))
    fails = [r for r in rows if (r[3] if len(r) > 3 else r[1]) == "FAIL"]
    print("FAIL count: %d" % len(fails))
    for r in fails:
        print("  FAIL -> %s" % (str(r)))


if __name__ == "__main__":
    main()
