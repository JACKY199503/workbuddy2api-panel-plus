"""模型编排（auto 虚拟模型 + 降级链）回归测试。

用法：python t_auto.py
所有用例走真实请求；改动配置用面板 /panel/api/config（热生效 + 落盘），
测完恢复初始配置。
"""
import json, time, urllib.request, urllib.error

import os as _os, sys as _sys
BASE = (_sys.argv[1] if len(_sys.argv) > 1 else _os.environ.get('WB2A_BASE', 'http://127.0.0.1:7863')).rstrip('/')
_KEY = _sys.argv[2] if len(_sys.argv) > 2 else _os.environ.get('WB2A_KEY', '')
K = _KEY
PASS, FAIL = [], []


def req(m, p, body=None, key=K, timeout=150, raw=False):
    d = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + p, data=d, method=m)
    r.add_header('Content-Type', 'application/json')
    r.add_header('Authorization', 'Bearer ' + key)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as x:
            txt = x.read().decode('utf-8', 'replace')
            if raw:
                return x.status, txt, dict(x.headers)
            return x.status, json.loads(txt), dict(x.headers)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw), dict(e.headers)
        except Exception:
            return e.code, {'raw': raw[:200]}, dict(e.headers)


def ok(name, cond, detail=''):
    (PASS if cond else FAIL).append(name)
    print(('  OK  ' if cond else ' FAIL ') + name + ('  ' + detail if detail else ''))


def chat(model, stream=False, mt=200, txt='用一句话介绍你自己'):
    return req('POST', '/v1/chat/completions', {
        'model': model, 'messages': [{'role': 'user', 'content': txt}],
        'max_tokens': mt, 'stream': stream}, raw=stream)


def routed(h):
    """响应头里的实际出站模型（Go 会把头名规范成 X-Wb2a-Routed-Model 形式）。"""
    for k, v in h.items():
        if k.lower() == 'x-wb2a-routed-model':
            return v
    return None


def content_of(j):
    ch = j.get('choices') or [{}]
    m = ch[0].get('message', {}) if ch else {}
    return (m.get('content') or '')


def get_cfg():
    st, j, _ = req('GET', '/panel/api/config')
    return j['config']


def set_cfg(patch):
    cur = get_cfg()
    for k, v in patch.items():
        cur[k] = v
    st, j, _ = req('POST', '/panel/api/config', cur)
    if st != 200:
        print('  !!! 保存配置失败', st, j)
    return st


orig = get_cfg()
orig_auto = json.loads(json.dumps(orig.get('auto_model', {})))
orig_mf = json.loads(json.dumps(orig.get('model_fallback', {})))
print('=== 0. 基线配置')
print('   auto_model:', json.dumps(orig_auto, ensure_ascii=False))
print('   model_fallback:', json.dumps(orig_mf, ensure_ascii=False))

# 测试链一律用「真实在册模型」，不用 fast-model / balanced-model 那类别名：
# 别名不在 /v1/models 列表里，且 2026-09-22 起上游让它们把 max_tokens 预算
# 全喂给 reasoning，返回 200 + 空正文 —— 会稳定触发 on_empty 降级，
# 让「编排到链首」这类断言变成环境噪声。真实模型名才是可重复的判据。
CHAIN = ['cn:hy3', 'cn:deepseek-v4.1-flash', 'cn:hy3-x']
BASE_AUTO = {
    'enabled': True, 'virtual_id': 'auto', 'override': True,
    'day_primary': 'cn:hy3', 'night_primary': 'cn:hy3',
    'day_start': 8, 'day_end': 23,
    'fallback': ['cn:deepseek-v4.1-flash', 'cn:hy3-x'],
    'on_empty': True,
}

print('\n=== 1. 白天时段：auto → day_primary')
set_cfg({'auto_model': BASE_AUTO, 'model_fallback': {}})
st, j, h = chat('auto')
ok('auto 非流式 200', st == 200, 'status=%s' % st)
# 链首偶发空正文时会按设计降级，故判「落在候选链内」而非死盯链首
ok('auto 编排落在候选链内', routed(h) in CHAIN, 'routed=%s' % routed(h))
ok('auto 有正文', len(content_of(j)) > 0, 'len=%d' % len(content_of(j)))

print('\n=== 2. 流式同样编排')
st, j, h = chat('auto', stream=True)
ok('auto 流式 200', st == 200, 'status=%s' % st)
ok('auto 流式有数据帧', 'data:' in (j or ''), 'frames=%d' % (j or '').count('data:'))
ok('auto 流式 routed 头', routed(h) in CHAIN,
   'routed=%s' % routed(h))

print('\n=== 3. cn:auto / global:auto')
st, j, h = chat('cn:auto')
ok('cn:auto → cn 候选链', st == 200 and routed(h) in CHAIN,
   'st=%s routed=%s' % (st, routed(h)))
st, j, h = chat('global:auto')
# global 侧无候选 → 原样直出（上游无裸 auto 的 global 模型 → 报错，属预期）
ok('global:auto 不误编排到 cn 模型',
   (routed(h) or '').startswith('global:') or routed(h) is None,
   'st=%s routed=%s' % (st, routed(h)))

print('\n=== 4. 主模型不可用 → 降级到 fallback[0]')
# on_empty=false：隔离「空回复降级」，只验证「主模型不可用」这一条降级路径
set_cfg({'auto_model': dict(BASE_AUTO, day_primary='cn:definitely-not-a-model', on_empty=False)})
st, j, h = chat('auto')
ok('主模型无号时降级', routed(h) in ('cn:deepseek-v4.1-flash', 'cn:hy3-x'),
   'st=%s routed=%s' % (st, routed(h)))

print('\n=== 5. 全链不可用 → 末端 503（不是 500/超时）')
set_cfg({'auto_model': dict(BASE_AUTO, day_primary='cn:nope-1', fallback=['cn:nope-2'])})
st, j, h = chat('auto')
ok('全链失败 503 no_healthy_account',
   st == 503 and (j.get('error') or {}).get('code') == 'no_healthy_account',
   'st=%s code=%s' % (st, (j.get('error') or {}).get('code')))

print('\n=== 6. 空回复降级（on_empty，issue #31 的 reasoning 坑）')
set_cfg({'auto_model': BASE_AUTO, 'model_fallback': {'cn:hy3': ['cn:deepseek-v4.1-flash']}})
st, j, h = chat('cn:hy3')
# cn:hy3 偶发空正文（reasoning 吃满预算），命中则必须被 on_empty 救回到下一候选；
# 未命中空正文时本来就有正文，两者都算通过。
ok('model_fallback 生效：空回复 → 下一候选',
   len(content_of(j)) > 0 and routed(h) in (None, 'cn:hy3', 'cn:deepseek-v4.1-flash'),
   'st=%s routed=%s len=%d' % (st, routed(h), len(content_of(j))))
# 关闭 on_empty 后：空回复原样返回，不降级
set_cfg({'auto_model': dict(BASE_AUTO, on_empty=False),
         'model_fallback': {'cn:hy3': ['cn:deepseek-v4.1-flash']}})
st, j, h = chat('cn:hy3')
ok('on_empty=false 时空回复不降级', routed(h) is None,
   'st=%s routed=%s' % (st, routed(h)))

print('\n=== 7. 编排关闭 = 零回归')
set_cfg({'auto_model': dict(BASE_AUTO, enabled=False), 'model_fallback': {}})
st, j, h = chat('cn:hy3')
ok('关闭后具名模型照常 200 且无 routed 头',
   st == 200 and routed(h) is None, 'st=%s' % st)
st, j, _ = req('GET', '/v1/models')
ids = [m['id'] for m in j['data']]
ok('关闭后 /v1/models 无虚拟条目', 'auto' not in ids, 'auto in ids=%s' % ('auto' in ids))

print('\n=== 8. 虚拟模型在 /v1/models 中列出（启用时）')
set_cfg({'auto_model': BASE_AUTO})
st, j, _ = req('GET', '/v1/models')
ids = [m['id'] for m in j['data']]
ok('启用后列出 auto', 'auto' in ids)
ok('无重复 id', len(ids) == len(set(ids)), 'total=%d uniq=%d' % (len(ids), len(set(ids))))

print('\n=== 9. 密钥白名单不被降级绕过')
# 建一把只允许 cn:hy3 的 cn 密钥，auto 的候选链里其余模型应被剔除
st, j, _ = req('POST', '/panel/api/keys',
               {'name': 'T-auto-wl', 'realm': 'cn', 'models': ['cn:hy3'], 'quota_credit': 5})
if st == 200:
    kid, plain = j['key']['id'], j['plain']
    st2, j2, h2 = req('POST', '/v1/chat/completions', {
        'model': 'auto', 'messages': [{'role': 'user', 'content': 'hi'}],
        'max_tokens': 100, 'stream': False}, key=plain)
    ok('白名单密钥：auto 只落在允许的模型上',
       routed(h2) in (None, 'cn:hy3'),
       'st=%s routed=%s' % (st2, routed(h2)))
    req('DELETE', '/panel/api/keys/' + kid)
else:
    ok('建测试密钥', False, json.dumps(j, ensure_ascii=False)[:120])

print('\n=== 恢复初始配置')
set_cfg({'auto_model': orig_auto, 'model_fallback': orig_mf})
print('   restored:', json.dumps(get_cfg().get('auto_model'), ensure_ascii=False))

print('\n==== %d 通过 / %d 失败 ====' % (len(PASS), len(FAIL)))
for f in FAIL:
    print('  FAILED:', f)
