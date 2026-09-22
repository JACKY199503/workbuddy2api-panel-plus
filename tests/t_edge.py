import json, urllib.request, urllib.error, time

import os as _os, sys as _sys
BASE = (_sys.argv[1] if len(_sys.argv) > 1 else _os.environ.get('WB2A_BASE', 'http://127.0.0.1:7863')).rstrip('/')
_KEY = _sys.argv[2] if len(_sys.argv) > 2 else _os.environ.get('WB2A_KEY', '')
K = _KEY
PASS, FAIL = [], []


def req(m, p, body=None, key=K):
    d = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + p, data=d, method=m)
    r.add_header('Content-Type', 'application/json')
    r.add_header('Authorization', 'Bearer ' + key)
    try:
        with urllib.request.urlopen(r, timeout=90) as x:
            return x.status, json.loads(x.read().decode())
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {'raw': raw[:200]}


def errcode(j):
    e = j.get('error')
    if isinstance(e, dict):
        return e.get('code') or e.get('message')
    if isinstance(e, str):
        return e
    return None


def chk(name, cond, detail=''):
    (PASS if cond else FAIL).append(name)
    print(('  OK  ' if cond else ' FAIL ') + name + ((' | ' + detail) if detail else ''))


def chat(key, model, mt=1, txt='hi'):
    st, j = req('POST', '/v1/chat/completions', {
        'model': model, 'messages': [{'role': 'user', 'content': txt}],
        'max_tokens': mt, 'stream': False}, key=key)
    return st, errcode(j)


def chat_credit(key, model):
    """真实计费请求（max_tokens 足够大，上游才会产生积分消耗），返回 (st, code, credit)。"""
    st, j = req('POST', '/v1/chat/completions', {
        'model': model, 'messages': [{'role': 'user', 'content': '用一句话介绍你自己'}],
        'max_tokens': 200, 'stream': False}, key=key)
    u = j.get('usage') or {}
    return st, errcode(j), (u.get('credit') or 0)


# 模型清单随上游变动，用例一律动态取（不写死模型名）；auto 是编排虚拟入口，跳过
_ALL = [d['id'] for d in req('GET', '/v1/models')[1]['data']]
_CN = [x for x in _ALL if x.startswith('cn:') and not x.endswith('auto')]
_GL = [x for x in _ALL if x.startswith('global:') and not x.endswith('auto')]
# global 域可能因账号冷却/额度耗尽临时为空 —— 全局降级为 None，相关用例跳过
GL1 = _GL[0] if _GL else None
CN1 = _CN[0]

# 计费模型：上游并非每个模型都产生积分（免费模型 credit=0，额度永远不会耗尽），
# 取第一个实测 credit>0 的模型作额度用例
BILL, BILL_HAS_CREDIT = CN1, False
for cand in _CN[:4]:
    s, c, cr = chat_credit(K, cand)
    if s == 200 and cr and cr > 0:
        BILL, BILL_HAS_CREDIT = cand, True
        break
print('用例模型：cn=%s global=%s 计费=%s' % (CN1, GL1, BILL))


def mk(name, **kw):
    body = {'name': name, 'realm': kw.get('realm', '')}
    for f in ('models', 'quota', 'quota_credit', 'expires_at'):
        if f in kw:
            body[f] = kw[f]
    st, j = req('POST', '/panel/api/keys', body)
    assert st == 200, (st, j)
    return j['plain'], j['key']


created = []
try:
    # 1) 跨版本调用拦截（回归确认）
    print('== 1 跨版本拦截 ==')
    p, k = mk('E-cn', realm='cn'); created.append(k['id'])
    st, code = chat(p, GL1)
    chk('cn 密钥调 global → 400 realm_mismatch', st == 400 and code == 'realm_mismatch', 'st=%s %s' % (st, code))
    p, k = mk('E-gl', realm='global'); created.append(k['id'])
    st, code = chat(p, CN1)
    chk('global 密钥调 cn → 400 realm_mismatch', st == 400 and code == 'realm_mismatch', 'st=%s %s' % (st, code))

    # 2a) 密钥过期
    print('== 2 到期/额度耗尽 ==')
    p, k = mk('E-expired', realm='cn', expires_at='2020-01-01T00:00:00Z'); created.append(k['id'])
    st, code = chat_credit(p, BILL)[:2]
    chk('过期密钥 → 403 key_expired', st == 403 and code == 'key_expired', 'st=%s %s' % (st, code))
    st2, j2 = req('GET', '/v1/models', key=p)
    chk('过期密钥 /v1/models → 403', st2 == 403, 'st=%s' % st2)

    # 2b) 积分耗尽（需要上游本次真的产生积分消耗；免费窗口 credit=0 时无法触发，
    #     该用例自动跳过并在结果里标注，避免把环境状态误报成功能缺陷）
    if not BILL_HAS_CREDIT:
        print('  --  跳过积分额度用例：当前上游对所有候选模型均返回 credit=0（未产生积分消耗）')
    else:
        p, k = mk('E-credit', realm='cn', quota_credit=0.01); created.append(k['id'])
        st, code = chat_credit(p, BILL)[:2]
        chk('积分额度 0.01 首次调用成功', st == 200, 'st=%s %s' % (st, code))
        # 单次消耗可能小于额度，连续调用直到额度耗尽（最多 6 次）
        st, code, used = 200, None, 0
        for _ in range(6):
            used += 1
            st, code = chat_credit(p, BILL)[:2]
            if st == 429:
                break
        chk('积分耗尽后 → 429 credit_quota_exhausted', st == 429 and code == 'credit_quota_exhausted',
            'st=%s %s 第%d次触发' % (st, code, used))
        st2, j2 = req('GET', '/v1/models', key=p)
        chk('积分耗尽 /v1/models → 429', st2 == 429, 'st=%s' % st2)
        st3, _ = req('POST', '/panel/api/keys/' + k['id'] + '/reset')
        st, code = chat_credit(p, BILL)[:2]
        chk('重置用量后恢复可用', st3 == 200 and st == 200, 'st=%s %s' % (st, code))

    # 2c) token 额度耗尽
    p, k = mk('E-token', realm='cn', quota=10); created.append(k['id'])
    st, code = chat_credit(p, BILL)[:2]
    chk('token 额度 10 首次调用成功', st == 200, 'st=%s %s' % (st, code))
    st, code = chat_credit(p, BILL)[:2]
    chk('token 耗尽后 → 429 quota_exhausted', st == 429 and code == 'quota_exhausted', 'st=%s %s' % (st, code))

    # 3) 不限版本密钥：全量模型 + 双版本联通
    print('== 3 不限版本联通 ==')
    p, k = mk('E-any', realm=''); created.append(k['id'])
    st, j = req('GET', '/v1/models', key=p)
    ids = sorted(d['id'] for d in j['data'])
    ncn = sum(1 for x in ids if not x.startswith('global:'))
    ngl = sum(1 for x in ids if x.startswith('global:'))
    chk('不限密钥拿到全量 cn + global', st == 200 and ids == sorted(_ALL),
        'cn=%d global=%d total=%d' % (ncn, ngl, len(ids)))
    st1, c1 = chat(p, CN1)
    st2, c2 = chat(p, GL1)
    chk('不限密钥 cn+global 模型均 200', st1 == 200 and st2 == 200, 'cn st=%s, global st=%s' % (st1, st2))
    st3, c3 = chat(p, _CN[1] if len(_CN) > 1 else CN1)
    st4, c4 = chat(p, _GL[1] if len(_GL) > 1 else GL1)
    # 上游账号池可能对某些模型无可用号（503）；这里只排除鉴权/配置类拦截（4xx）
    chk('不限密钥第二个 cn/global 模型不被鉴权拦截', st3 not in (400, 401, 403, 429) and st4 not in (400, 401, 403, 429), 'cn st=%s, global st=%s' % (st3, st4))
finally:
    for kid in created:
        req('DELETE', '/panel/api/keys/' + kid)

print('\nPASS %d / FAIL %d' % (len(PASS), len(FAIL)))
for f in FAIL:
    print('  ✗', f)
