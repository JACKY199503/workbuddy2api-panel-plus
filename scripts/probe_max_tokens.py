#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
OpenAI 兼容端点的「真实输出上限」探测工具
==========================================
背景：许多网关/中转会在 /v1/models 里透出 max_output_tokens，但它常常与实际能力不符
（实测某网关对 deepseek-v4.1-flash 声称 393216，真实只有 32000）。更麻烦的是**静默钳制**：
请求 max_tokens 超过真实上限时不报错，而是内部截断——从 usage 只能看出"输出到某个值就停了"。

本工具用「强制长输出 + 阶梯上探」测出真实上限，关键判据区分两种停止：

  finish=length 且 输出 < 请求  → 上游截断，**输出值即真实上限**（可信，直接得出结果）
  finish=length 且 输出 = 请求  → 支持至少这么多，继续上探更高阶梯
  finish=stop   且 输出 < 请求  → 模型自己不想写了，**不可作判据**，需强化提示重试

用法：
  # 测某网关全部以给定前缀开头的模型（默认 cn:）
  python probe_max_tokens.py --base http://211.154.25.123:7863/v1 --key sk-xxx

  # 指定模型 / 多个前缀 / 只看计划
  python probe_max_tokens.py --base ... --key ... --models flash,glm-5.3
  python probe_max_tokens.py --base ... --key ... --prefix cn: --prefix global:
  python probe_max_tokens.py --base ... --key ... --dry-run

  # 控制成本与时长
  python probe_max_tokens.py --base ... --key ... --tiers 40000,100000 --retries 2 --timeout 900

结果：
  - 控制台表格：模型 | 声称上限 | 实测上限 | 判据 | 耗时
  - JSONL 落盘（--out，默认 probe-max-tokens.jsonl），**支持 --resume 断点续测**
  - --panel-out PATH 额外写面板契约文件（默认关闭；网关面板「模型与档位」的实测列读它）：
      {"version":1,"probes":{"cn:glm-5.2":{"claimed":131072,"measured":48000,
       "verdict":"clamped|at_least|inconclusive","note":"原始判据文本",
       "tested_at":"...","source":"probe_max_tokens.py"}}}
    写入为合并语义：未重测的模型保留旧记录；文件放网关 state 文件同目录
    （默认部署即 data/output_probes.json），面板下次查询即展示，网关零重启。
  - --limit N 只测前 N 个模型（先小批量试跑）

成本提示：一个模型若真有极大上限，上探到高档位会生成数十万 token，耗时长且消耗额度。
        默认最高阶梯 40000、上限 160000 已足够识别常见的 32K/48K/64K 钳制。
"""

import argparse
import json
import os
import re
import socket
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

# 强制长输出的提示：目标值需与被测档位"量级相当"。
# 实测教训：目标写得过大（数到 999999）模型会判断"不可能完成"而提前收尾（只输出 1~3k）；
# 目标设为 12000~20000（能写满但看起来合理）时，模型会持续输出直到被硬截断，4/4 稳定命中。
# 下面按档位缩放目标值：约 max_tokens/3 个数字（每数字行约 2~3 token，留余量防提前写完）。
def long_prompt(max_tokens: int) -> str:
    target = max(2000, min(200000, max_tokens // 3))
    return (
        f"请从 1 开始，每行输出一个数字，依次递增，一直数到 {target}。"
        "严格只输出数字本身，每行一个；不要解释、不要总结、不要省略、不要合并、不要提前结束。"
    )


# 强化版（第一次被模型主动收尾时使用，仍保持量级匹配）
def long_prompt_hard(max_tokens: int) -> str:
    target = max(2000, min(200000, max_tokens // 3))
    return (
        f"Count from 1 to {target}, one number per line, incrementing by 1. "
        "Output digits only, one per line. Do not summarize, do not explain, "
        "do not skip numbers, do not stop early — you must reach the target."
    )


def fetch_models(base: str, key: str, timeout: int = 20):
    """取 /v1/models 列表：[{id, claimed_output, claimed_ctx}, ...]"""
    url = base.rstrip("/") + "/models"
    req = urllib.request.Request(url, headers={
        "Authorization": f"Bearer {key}", "x-api-key": key, "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        data = json.loads(r.read().decode("utf-8"))
    items = data.get("data") if isinstance(data, dict) else data
    out = []
    for it in items or []:
        if isinstance(it, str):
            out.append({"id": it.strip(), "claimed_output": None, "claimed_ctx": None})
            continue
        if not isinstance(it, dict):
            continue
        mid = (it.get("id") or it.get("name") or "").strip()
        if not mid:
            continue
        top = it.get("top_provider") if isinstance(it.get("top_provider"), dict) else {}
        out.append({
            "id": mid,
            "claimed_output": it.get("max_output_tokens") or it.get("max_completion_tokens")
                              or top.get("max_completion_tokens"),
            "claimed_ctx": it.get("context_length") or it.get("max_input_tokens"),
        })
    return out


def probe_once(base: str, key: str, model: str, max_tokens: int, prompt: str, timeout: int):
    """发一次强制长输出请求，返回 (out_tokens, reasoning_tokens, finish, err)。"""
    body = json.dumps({
        "model": model, "stream": True, "max_tokens": max_tokens,
        "messages": [{"role": "user", "content": prompt}],
    }).encode()
    req = urllib.request.Request(base.rstrip("/") + "/chat/completions", data=body, headers={
        "Authorization": f"Bearer {key}", "x-api-key": key,
        "Content-Type": "application/json", "Accept": "text/event-stream"}, method="POST")
    out = None
    reasoning = None
    finish = None
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            buf = ""
            while True:
                piece = r.read(8192)
                if not piece:
                    break
                buf += piece.decode("utf-8", "replace")
                while "\n" in buf:
                    line, buf = buf.split("\n", 1)
                    line = line.strip()
                    if not line.startswith("data:"):
                        continue
                    payload = line[5:].strip()
                    if not payload or payload == "[DONE]":
                        continue
                    try:
                        obj = json.loads(payload)
                    except Exception:
                        continue
                    u = obj.get("usage")
                    if isinstance(u, dict):
                        out = u.get("completion_tokens")
                        det = u.get("completion_tokens_details")
                        if isinstance(det, dict):
                            reasoning = det.get("reasoning_tokens")
                    for c in obj.get("choices") or []:
                        if isinstance(c, dict) and c.get("finish_reason"):
                            finish = c["finish_reason"]
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:300]
        return None, None, None, f"HTTP {e.code}: {detail}"
    except Exception as e:
        return None, None, None, f"{type(e).__name__}: {str(e)[:150]}"
    return out, reasoning, finish, None


def probe_model(base: str, key: str, model: str, tiers: list[int],
                retries: int, timeout: int, quiet: bool, budget: int = 0) -> dict:
    """阶梯上探单个模型，返回结果字典。
    budget>0 时为单模型总时间预算（秒）：超出即中止并记录"未测完"，避免个别大上限模型
    把整体拖到几十分钟（实测某模型生成 40000 token 需 720s，上探 100000 档要半小时）。"""
    result = {"model": model, "measured": None, "verdict": None, "evidence": []}
    started = time.time()
    all_failed = True
    for tier in tiers:
        if budget and time.time() - started > budget:
            result.update(verdict=f"未测完（超出时间预算 {budget}s；已知至少 >{result.get('max_full') or 0}）")
            result["elapsed"] = round(time.time() - started, 1)
            return result
        tier_ok = False
        for attempt in range(1, retries + 1):
            prompt = long_prompt(tier) if attempt == 1 else long_prompt_hard(tier)
            # 单次请求也受剩余预算约束，防止单请求无上限地跑
            req_timeout = timeout
            if budget:
                remain = budget - (time.time() - started)
                if remain <= 5:
                    break
                req_timeout = int(min(timeout, remain))
            t0 = time.time()
            out, reasoning, finish, err = probe_once(base, key, model, tier, prompt, req_timeout)
            dt = time.time() - t0
            if err:
                result["evidence"].append(f"tier={tier} 请求失败: {err[:120]}")
                if not quiet:
                    print(f"      [{model}] tier={tier}: 失败 {err[:70]}", flush=True)
                break
            tier_ok = True
            all_failed = False
            rs = f" 推理{reasoning}" if reasoning else ""
            if not quiet:
                print(f"      [{model}] tier={tier} #{attempt}: 输出 {out} tok{rs} "
                      f"finish={finish} {dt:.0f}s", flush=True)

            if finish == "length" and out is not None:
                if out < tier:
                    # 上游截断。注意：若上一档已满额通过（max_full），说明真实上限在
                    # [max_full, out] 之间，语义是"≥out 附近被钳"，用区间表达更准确。
                    prev = result.get("max_full")
                    if prev and out < prev:
                        verdict = f"≈{out}（钳制；上一档满额 {prev}，取值 {out}）"
                    else:
                        verdict = "钳制"
                    result.update(measured=out, verdict=verdict,
                                  evidence=result["evidence"] + [f"要 {tier} 只给 {out}（finish=length）"])
                    result["elapsed"] = round(time.time() - started, 1)
                    return result
                result["evidence"].append(f"tier={tier}: 输出满额 {out}（支持≥{tier}）")
                result["max_full"] = out
                break
            if finish == "stop":
                result["evidence"].append(f"tier={tier} #{attempt}: finish=stop（模型主动收尾，输出 {out}）")
                if out is not None:
                    result["max_full"] = max(result.get("max_full") or 0, out)
                if attempt == retries:
                    # 已满额通过的档位仍然有效：结论是"≥max_full"，不是"无结论"
                    if result.get("max_full"):
                        result.update(measured=None,
                                      verdict=f"≥{result['max_full']}（模型主动收尾，未测到钳制点；"
                                              f"声称 {result.get('claimed_output') or '?'}）")
                    else:
                        result.update(verdict="模型主动停止（未能测出上限，非钳制）")
                    result["elapsed"] = round(time.time() - started, 1)
                    return result
                continue
            result["evidence"].append(f"tier={tier}: 异常返回 out={out} finish={finish}")
            break

    if all_failed:
        result.update(measured=None, verdict="全部请求失败（模型/账号不可用，无结论）")
        result["elapsed"] = round(time.time() - started, 1)
        return result
    if result.get("max_full"):
        result.update(measured=result["max_full"],
                      verdict=f"≥{result['max_full']}（最高档位仍满额，未触顶）")
    result["elapsed"] = round(time.time() - started, 1)
    return result


def classify(res: dict):
    """把一条探测记录映射为面板契约五态之三：clamped / at_least / inconclusive。

    verdict 原文是自由中文文本，这里按前缀归类：
      钳制 / ≈N（钳制…）       → clamped，measured = 截断值（可信）
      ≥N（…）/ 未测完（…至少 >N）→ at_least，measured = 已知下界
      其余（主动停止/全部失败）  → inconclusive
    下界取值链：measured → max_full → 文本里的 ≥N / >N。"""
    v = res.get("verdict") or ""
    meas = res.get("measured")
    if v.startswith("钳制") or v.startswith("≈"):
        return "clamped", meas
    floor = meas if isinstance(meas, int) else res.get("max_full")
    if not floor:
        m = re.search(r"[≥>]\s*(\d+)", v)
        floor = int(m.group(1)) if m else None
    if v.startswith("≥") or v.startswith("未测完"):
        return ("at_least", floor) if floor else ("inconclusive", None)
    return "inconclusive", None


def write_panel_out(path: str, results: list) -> None:
    """写面板契约文件（合并语义：未重测的模型保留旧记录，上游改限后重跑即覆盖）。"""
    p = Path(path)
    contract = {"version": 1, "probes": {}}
    if p.is_file():
        try:
            old = json.loads(p.read_text(encoding="utf-8"))
            if isinstance(old.get("probes"), dict):
                contract["probes"].update(old["probes"])
        except Exception:
            pass  # 旧文件损坏则整体重建，不让坏文件卡住新结果
    for r in results:
        verdict, meas = classify(r)
        contract["probes"][r["model"]] = {
            "claimed": r.get("claimed_output"),
            "measured": meas,
            "verdict": verdict,
            "note": r.get("verdict") or "",
            "tested_at": r.get("time") or "",
            "source": "probe_max_tokens.py",
        }
    tmp = p.with_suffix(p.suffix + ".tmp")
    tmp.parent.mkdir(parents=True, exist_ok=True)
    tmp.write_text(json.dumps(contract, ensure_ascii=False, indent=1), encoding="utf-8")
    tmp.replace(p)
    print(f"面板探测数据已写入 {p}（共 {len(contract['probes'])} 条，合并保留未重测模型）")


def main() -> None:
    ap = argparse.ArgumentParser(
        description="探测 OpenAI 兼容端点的真实输出上限（区分静默钳制与模型主动停止）")
    ap.add_argument("--base", required=True, help="接口基址，如 http://host:7863/v1")
    ap.add_argument("--key", default=None, help="API Key（也可用环境变量 PROBE_API_KEY）")
    ap.add_argument("--prefix", action="append", default=None,
                    help="只测以此前缀开头的模型（可多次；默认 cn:）")
    ap.add_argument("--models", default=None, help="按子串匹配模型 id（逗号分隔，优先级高于 --prefix）")
    ap.add_argument("--tiers", default="40000,100000",
                    help="阶梯请求值，逗号分隔（默认 40000,100000；越高越慢越费额度）")
    ap.add_argument("--retries", type=int, default=3, help="finish=stop 时的重试次数（默认 3）")
    ap.add_argument("--timeout", type=int, default=900, help="单次请求超时秒数（默认 900）")
    ap.add_argument("--limit", type=int, default=0, help="只测前 N 个模型（0=全部）")
    ap.add_argument("--out", default="probe-max-tokens.jsonl", help="结果 JSONL 路径")
    ap.add_argument("--panel-out", default=None,
                    help="额外写网关面板契约文件（如 data/output_probes.json；默认关闭）。"
                         "合并写入：未重测的模型保留旧记录")
    ap.add_argument("--resume", action="store_true", help="跳过已有结果的模型（断点续测）")
    ap.add_argument("--jobs", type=int, default=4,
                    help="并行探测的模型数（默认 4；瓶颈是墙钟等待，并行可大幅缩短总时长）")
    ap.add_argument("--budget", type=int, default=600,
                    help="单模型时间预算秒数（默认 600；超出记「未测完」，避免大上限模型拖垮整体）")
    ap.add_argument("--dry-run", action="store_true", help="只列出将要测的模型与阶梯，不发请求")
    ap.add_argument("--quiet", action="store_true", help="不打印每次尝试明细")
    args = ap.parse_args()

    key = args.key or os.environ.get("PROBE_API_KEY") or ""
    if not key and not args.dry_run:
        raise SystemExit("[!] 需要 --key 或环境变量 PROBE_API_KEY")

    tiers = [int(x) for x in args.tiers.split(",") if x.strip()]
    tiers.sort()

    socket.setdefaulttimeout(args.timeout)

    print(f"目标: {args.base}")
    all_models = fetch_models(args.base, key)
    if args.models:
        kws = [k.strip().lower() for k in args.models.split(",") if k.strip()]
        def _match(mid: str) -> int:
            """返回匹配优先级：0=不匹配，3=带前缀的完整 id，2=后缀精确，1=子串。
            关键词含 ':' 时按完整 id 比对（cn:xxx 只命中 cn:xxx，不含 global:xxx）。"""
            low = mid.lower()
            for k in kws:
                if ":" in k and low == k:
                    return 3
            for k in kws:
                if ":" not in k and (low == k or low.endswith(":" + k)):
                    return 2
            for k in kws:
                if k in low:
                    return 1
            return 0
        scored = [(m, _match(m["id"])) for m in all_models]
        best = max((s for _, s in scored), default=0)
        targets = [m for m, s in scored if s == best and s > 0]
    else:
        prefixes = args.prefix or ["cn:"]
        targets = [m for m in all_models if any(m["id"].startswith(p) for p in prefixes)]
    if args.limit:
        targets = targets[:args.limit]
    if not targets:
        raise SystemExit(f"[!] 没有匹配的模型（共 {len(all_models)} 个可测）")

    done = {}
    out_path = Path(args.out)
    if args.resume and out_path.is_file():
        for line in out_path.read_text(encoding="utf-8").splitlines():
            try:
                rec = json.loads(line)
                done[rec["model"]] = rec
            except Exception:
                pass

    print(f"待测模型 {len(targets)} 个 | 阶梯 {tiers} | 声称上限来自 /v1/models")
    if args.dry_run:
        print("\n[dry-run] 计划：")
        for m in targets:
            skip = "  (已有结果，将跳过)" if m["id"] in done else ""
            print(f"  {m['id']:44} 声称 {str(m['claimed_output'] or '-'):>8}{skip}")
        return

    print()
    results = []
    todo = [m for m in targets if m["id"] not in done]
    for m in targets:
        if m["id"] in done:
            results.append(done[m["id"]])

    # 并行探测：墙钟时间是瓶颈（单模型生成 4 万 token 需数分钟），并发能有效压缩总时长。
    # 结果仍按模型去重，落盘用锁保护。
    import concurrent.futures as cf
    import threading
    write_lock = threading.Lock()
    finished = {"n": 0}

    def _work(m):
        res = probe_model(args.base, key, m["id"], tiers, args.retries, args.timeout,
                          args.quiet, budget=args.budget)
        res["claimed_output"] = m["claimed_output"]
        res["claimed_ctx"] = m["claimed_ctx"]
        res["time"] = time.strftime("%Y-%m-%d %H:%M:%S")
        with write_lock:
            finished["n"] += 1
            n = finished["n"]
            with out_path.open("a", encoding="utf-8") as f:
                f.write(json.dumps(res, ensure_ascii=False) + "\n")
        measured = res["measured"] if res["measured"] is not None else "-"
        print(f"[{n}/{len(todo)}] {m['id']}（声称 {m['claimed_output'] or '-'}）"
              f" → 实测 {measured} | {res['verdict']} | {res.get('elapsed','-')}s", flush=True)
        return res

    if todo:
        print(f"开始探测 {len(todo)} 个模型（并行 {args.jobs}，单模型预算 {args.budget}s）...\n")
        with cf.ThreadPoolExecutor(max_workers=max(1, args.jobs)) as ex:
            results.extend(ex.map(_work, todo))

    # 汇总表
    print("=" * 96)
    print(f"{'模型':44} {'声称':>9} {'实测':>9}  {'判据':<28} {'耗时':>6}")
    print("-" * 96)
    for r in results:
        claim = r.get("claimed_output") or "-"
        meas = r.get("measured") if r.get("measured") is not None else "-"
        verdict = (r.get("verdict") or "")[:26]
        print(f"{r['model']:44} {str(claim):>9} {str(meas):>9}  {verdict:<28} {str(r.get('elapsed','-')):>6}")
    print("=" * 96)

    mism = [r for r in results if r.get("measured") and r.get("claimed_output")
            and int(r["measured"]) < int(r["claimed_output"])]
    if mism:
        print(f"\n⚠ {len(mism)} 个模型的声称上限高于实测（静默钳制）：")
        for r in mism:
            print(f"    {r['model']}: 声称 {r['claimed_output']} → 实际 {r['measured']}")
    print(f"\n结果已写入 {out_path}（--resume 可跳过已测模型继续）")
    if args.panel_out:
        write_panel_out(args.panel_out, results)


if __name__ == "__main__":
    main()
