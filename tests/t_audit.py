# -*- coding: utf-8 -*-
"""编排 + 密钥分发 交叉审计：虚拟模型与白名单/版本归属的相互作用。

覆盖此前测试没覆盖的交叉面（改这两处后回归用）：
  1. 虚拟模型在 /v1/models 里是否按密钥 realm 裁剪（cn/global 不串）
  2. 白名单填虚拟名（auto）时是否还能用（此前会 400 model_not_allowed）
  3. 白名单填具体模型名时降级是否会被掏空 / 是否绕过白名单
  4. 版本归属是否会在降级链上被绕过
"""
import json
import urllib.request
import urllib.error

import os as _os, sys as _sys
BASE = (_sys.argv[1] if len(_sys.argv) > 1 else _os.environ.get('WB2A_BASE', 'http://127.0.0.1:7863')).rstrip('/')
_KEY = _sys.argv[2] if len(_sys.argv) > 2 else _os.environ.get('WB2A_KEY', '')
K = _KEY

PASS = [0]
FAIL = []


def req(m, p, body=None, key=K, timeout=150):
    d = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + p, data=d, method=m)
    r.add_header('Content-Type', 'application/json')
    r.add_header('Authorization', 'Bearer ' + key)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as x:
            return x.status, json.loads(x.read().decode()), dict(x.headers)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw), dict(e.headers)
        except Exception:
            return e.code, {'raw': raw[:200]}, dict(e.headers)


def ok(name, cond, extra=''):
    if cond:
        PASS[0] += 1
        print('  OK  %-52s %s' % (name, extra))
    else:
        FAIL.append(name)
        print('  XX  %-52s %s' % (name, extra))


def errcode(j):
    e = j.get('error')
    return e.get('code') if isinstance(e, dict) else e


def routed(h):
    for k, v in h.items():
        if k.lower() == 'x-wb2a-routed-model':
            return v
    return None


def get_cfg():
    st, j, _ = req('GET', '/panel/api/config')
    return j['config']


def set_cfg(patch):
    c = get_cfg()
    for k, v in patch.items():
        c[k] = v
    st, j, _ = req('POST', '/panel/api/config', c)
    if st != 200:
        print('  !! 保存配置失败 st=%s %s' % (st, json.dumps(j, ensure_ascii=False)[:200]))
    return st


def mk(name, **kw):
    st, j, _ = req('POST', '/panel/api/keys', dict(name=name, **kw))
    return j['key']['id'], j['plain']


def rm(kid):
    req('DELETE', '/panel/api/keys/' + kid)


def models(key):
    st, j, _ = req('GET', '/v1/models', key=key)
    return st, [m['id'] for m in j.get('data', [])] if st == 200 else []


def chat(model, key=K, mt=60):
    return req('POST', '/v1/chat/completions', {
        'model': model, 'messages': [{'role': 'user', 'content': 'hi'}],
        'max_tokens': mt, 'stream': False}, key=key)


# ---- 取当前真实模型清单，挑出稳定可用的 cn / global 模型 ----
st, j, _ = req('GET', '/v1/models')
ALL = sorted(m['id'] for m in j['data'])
CN = [x for x in ALL if x.startswith('cn:') and not x.endswith(':auto')]
GL = [x for x in ALL if x.startswith('global:') and not x.endswith(':auto')]
CN1, CN2 = CN[0], (CN[1] if len(CN) > 1 else CN[0])
GL1 = GL[0] if GL else 'global:hy3'
print('模型清单: cn=%d global=%d | CN1=%s CN2=%s GL1=%s\n' % (len(CN), len(GL), CN1, CN2, GL1))

BASE_AUTO = get_cfg().get('auto_model') or {}
ORIG = dict(BASE_AUTO)
TEST_AUTO = dict(BASE_AUTO)
TEST_AUTO.update({
    'enabled': True, 'virtual_id': 'auto', 'override': True, 'on_empty': True,
    'day_primary': CN1, 'night_primary': CN1, 'day_start': 0, 'day_end': 23,
    'fallback': [CN2, GL1],
})
created = []

print('=== 1. 虚拟模型在列表里的 realm 裁剪 ===')
set_cfg({'auto_model': TEST_AUTO})
st, ids = models(K)
ok('管理员能看到 auto 与 cn:auto', 'auto' in ids and 'cn:auto' in ids, str([i for i in ids if i.endswith('auto')]))
ok('列表无重复 id', len(ids) == len(set(ids)))

kid_cn, p_cn = mk('AUD-cn', realm='cn')
created.append(kid_cn)
st, ids_cn = models(p_cn)
autos_cn = [i for i in ids_cn if i.endswith(':auto') or i == 'auto']
gl_cn = [i for i in ids_cn if i.startswith('global:')]
ok('cn 密钥列表含 auto/cn:auto', 'auto' in ids_cn and 'cn:auto' in ids_cn, str(autos_cn))
ok('cn 密钥列表不含 global:auto', 'global:auto' not in ids_cn, str(autos_cn))
ok('cn 密钥列表不含 global 模型', not gl_cn, str(gl_cn[:3]))

kid_gl, p_gl = mk('AUD-gl', realm='global')
created.append(kid_gl)
st, ids_gl = models(p_gl)
autos_gl = [i for i in ids_gl if i.endswith(':auto') or i == 'auto']
ok('global 密钥列表不含任何 auto（虚名 realm=cn）', not autos_gl, str(autos_gl))

st, j, _ = chat('auto', key=p_gl)
ok('global 密钥调 auto → 400 realm_mismatch', st == 400 and errcode(j) == 'realm_mismatch', 'st=%s %s' % (st, errcode(j)))
st, j, h = chat('auto', key=p_cn)
ok('cn 密钥调 auto → 200', st == 200, 'st=%s %s routed=%s' % (st, errcode(j), routed(h)))

print('\n=== 2. 白名单填虚拟名（auto）===')
kid2, p2 = mk('AUD-wl-auto', realm='cn', models=['auto'])
created.append(kid2)
st, ids2 = models(p2)
ok('白名单=[auto] → 列表只剩 auto 类（含 cn:auto 裸名同义）', st == 200 and ids2 and all(i == 'auto' or i == 'cn:auto' for i in ids2), str(ids2))
st, j, h = chat('auto', key=p2)
ok('白名单=[auto] 调 auto → 200（不再 400）', st == 200, 'st=%s %s' % (st, errcode(j)))
ok('routed 落在 cn 模型上', st != 200 or (routed(h) or '').startswith('cn:'), 'routed=%s' % routed(h))

print('\n=== 3. 白名单填具体模型名：不绕过、不掏空 ===')
kid3, p3 = mk('AUD-wl-one', realm='cn', models=[CN1])
created.append(kid3)
st, j, h = chat('auto', key=p3)
ok('白名单=[%s] 调 auto → 200（链首在名单内）' % CN1, st == 200, 'st=%s %s' % (st, errcode(j)))
ok('实际出站就是白名单内那个模型', routed(h) == CN1, 'routed=%s' % routed(h))

# 主模型不在白名单 → 链首就不合规 → 400（降级不得绕过白名单）
AUTO_NOPE = dict(TEST_AUTO)
AUTO_NOPE['day_primary'] = 'cn:definitely-not-a-model'
AUTO_NOPE['night_primary'] = 'cn:definitely-not-a-model'
set_cfg({'auto_model': AUTO_NOPE})
st, j, h = chat('auto', key=p3)
ok('主模型不在白名单 → 400 model_not_allowed（不降级绕过）',
   st == 400 and errcode(j) == 'model_not_allowed', 'st=%s %s' % (st, errcode(j)))

print('\n=== 4. 降级链不得绕过版本归属 ===')
set_cfg({'auto_model': AUTO_NOPE})
st, j, h = chat('auto', key=p2)  # 白名单=[auto]，realm=cn，fallback 含 global
ok('cn 密钥 + fallback 含 global → 不落到 global', st != 200 or (routed(h) or '').startswith('cn:'),
   'st=%s routed=%s' % (st, routed(h)))
ok('全 cn 候选失败 → 503（未偷偷用 global 成功）', st == 503 or (routed(h) or '').startswith('cn:'),
   'st=%s code=%s' % (st, errcode(j)))

# 无白名单的 cn 密钥（原路径）同样不得串域
st, j, h = chat('auto', key=p_cn)
ok('无白名单 cn 密钥同样不串 global', st != 200 or (routed(h) or '').startswith('cn:'),
   'st=%s routed=%s' % (st, routed(h)))

print('\n=== 5. 恢复配置 + 管理员零回归 ===')
set_cfg({'auto_model': ORIG})
st, j, h = chat(CN1)
ok('管理员调真实模型仍 200', st == 200, 'st=%s' % st)
st, j, h = chat('auto')
ok('管理员调 auto 仍 200', st == 200, 'st=%s routed=%s' % (st, routed(h)))
st, ids = models(K)
ok('恢复后列表无重复 id', len(ids) == len(set(ids)), 'total=%d' % len(ids))

for kid in created:
    rm(kid)

print('\nPASS %d / FAIL %d' % (PASS[0], len(FAIL)))
if FAIL:
    for f in FAIL:
        print('  FAILED:', f)
