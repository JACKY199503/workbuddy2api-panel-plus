import json, urllib.request, urllib.error

import os as _os, sys as _sys
BASE = (_sys.argv[1] if len(_sys.argv) > 1 else _os.environ.get('WB2A_BASE', 'http://127.0.0.1:7863')).rstrip('/')
_KEY = _sys.argv[2] if len(_sys.argv) > 2 else _os.environ.get('WB2A_KEY', '')
K = _KEY
PASS = []
FAIL = []


def req(m, p, body=None, key=K):
    d = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + p, data=d, method=m)
    r.add_header('Content-Type', 'application/json')
    r.add_header('Authorization', 'Bearer ' + key)
    try:
        with urllib.request.urlopen(r, timeout=60) as x:
            return x.status, json.loads(x.read().decode())
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {'raw': raw[:300]}


def chk(name, cond, detail=''):
    (PASS if cond else FAIL).append(name)
    print(('  OK  ' if cond else ' FAIL ') + name + ((' | ' + detail) if detail else ''))


def realm_of(mid):
    i = mid.find(':')
    if i < 0:
        return 'cn'
    p = mid[:i]
    return p if p in ('cn', 'global') else 'cn'


st, j = req('GET', '/v1/models')
ALL = [d['id'] for d in j['data']]
CN = sorted(x for x in ALL if realm_of(x) == 'cn')
GL = sorted(x for x in ALL if realm_of(x) == 'global')
# 白名单用例取列表里现存的第一个 cn 模型（模型清单会随上游变动，不写死模型名）；
# 跳过 auto 虚拟模型（它是编排入口，不是具体模型）
ONE = [x for x in CN if not x.endswith('auto')][0]


def mk(name, **kw):
    body = {'name': name, 'realm': kw.get('realm', ''), 'models': kw.get('models', [])}
    st, j = req('POST', '/panel/api/keys', body)
    assert st == 200, (st, j)
    return j['plain'], j['key']


def ids_of(key):
    st, j = req('GET', '/v1/models', key=key)
    return sorted(d['id'] for d in j['data']), st


created = []
try:
    p1, k1 = mk('T-CN-ALL', realm='cn');            created.append(k1['id'])
    p2, k2 = mk('T-GL-ALL', realm='global');        created.append(k2['id'])
    p3, k3 = mk('T-ANY-ALL', realm='');             created.append(k3['id'])
    p4, k4 = mk('T-CN-ONE', realm='cn', models=[ONE]); created.append(k4['id'])

    print('\n== 模型列表裁剪 ==')
    ids, st = ids_of(p1)
    chk('cn 密钥 /v1/models 只含 cn 模型', st == 200 and len(ids) == len(CN) and ids == CN,
        'got %d, want %d, 混入 global=%s' % (len(ids), len(CN), [x for x in ids if realm_of(x) == 'global']))
    ids, st = ids_of(p2)
    chk('global 密钥 /v1/models 只含 global 模型', st == 200 and ids == GL,
        'got %d, want %d, 混入 cn=%s' % (len(ids), len(GL), [x for x in ids if realm_of(x) == 'cn']))
    ids, st = ids_of(p3)
    chk('不限版本密钥拿到全量模型', st == 200 and ids == sorted(ALL), 'got %d want %d' % (len(ids), len(ALL)))
    ids, st = ids_of(p4)
    chk('白名单单个模型 → 列表只剩 1 个', st == 200 and ids == [ONE], str(ids))
    ids, st = ids_of(K)
    chk('管理员 api_key 仍拿全量（零回归）', st == 200 and ids == sorted(ALL), 'got %d' % len(ids))

    print('\n== 跨版本调用拦截 ==')
    # global 域账号可能整体冷却（列表为空）：此时跳过依赖 global 模型的用例，
    # 不算功能失败（判据与 t_issue31 的 is_upstream_dry 同口径）。
    cases = [('cn 密钥调 global 模型', p1, GL[0] if GL else None),
             ('global 密钥调 cn 模型', p2, CN[0])]
    for label, key, bad in cases:
        if bad is None:
            print('  SKIP %s —— global 域当前无可用模型（账号冷却/额度耗尽）' % label)
            continue
        st, j = req('POST', '/v1/chat/completions', {
            'model': bad, 'messages': [{'role': 'user', 'content': 'hi'}], 'max_tokens': 1, 'stream': False,
        }, key=key)
        code = (j.get('error') or {}).get('code') if isinstance(j.get('error'), dict) else j.get('error')
        chk('%s → 400 realm_mismatch' % label, st == 400 and code == 'realm_mismatch', 'st=%s code=%s' % (st, code))

    print('\n== 白名单外调用 ==')
    st, j = req('POST', '/v1/chat/completions', {
        'model': CN[1], 'messages': [{'role': 'user', 'content': 'hi'}], 'max_tokens': 1, 'stream': False,
    }, key=p4)
    code = (j.get('error') or {}).get('code') if isinstance(j.get('error'), dict) else j.get('error')
    chk('白名单外模型 → 400 model_not_allowed', st == 400 and code == 'model_not_allowed', 'st=%s code=%s' % (st, code))
finally:
    for kid in created:
        req('DELETE', '/panel/api/keys/' + kid)

print('\nCN=%d GLOBAL=%d' % (len(CN), len(GL)))
print('PASS %d / FAIL %d' % (len(PASS), len(FAIL)))
for f in FAIL:
    print('  ✗', f)
