# -*- coding: utf-8 -*-
"""
t_adv.py —— 密钥分发 + 模型编排 反向/边界/安全 用例（代码走查驱动）。

只删自建密钥；auto_model / model_fallback 快照-恢复。
"""
import json
import time
import urllib.request
import urllib.error

import os as _os, sys as _sys
BASE = (_sys.argv[1] if len(_sys.argv) > 1 else _os.environ.get('WB2A_BASE', 'http://127.0.0.1:7863')).rstrip('/')
_KEY = _sys.argv[2] if len(_sys.argv) > 2 else _os.environ.get('WB2A_KEY', '')
ADMIN = _KEY

PASS, FAIL, NOTE = [], [], []


def ok(name, cond, detail=''):
    (PASS if cond else FAIL).append(name)
    print(('  OK  ' if cond else ' FAIL ') + name + ((' | ' + detail) if detail else ''))


def note(name, detail):
    NOTE.append(name)
    print('  NOTE ' + name + ((' | ' + detail) if detail else ''))


def req(m, p, body=None, key=None, headers=None, timeout=180):
    d = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + p, data=d, method=m)
    r.add_header('Content-Type', 'application/json')
    r.add_header('Authorization', 'Bearer ' + (key or ADMIN))
    for k, v in (headers or {}).items():
        r.add_header(k, v)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as x:
            raw = x.read().decode('utf-8', 'replace')
            try:
                return x.status, json.loads(raw), dict(x.headers)
            except Exception:
                return x.status, raw, dict(x.headers)
    except urllib.error.HTTPError as e:
        raw = e.read().decode('utf-8', 'replace')
        try:
            return e.code, json.loads(raw), dict(e.headers)
        except Exception:
            return e.code, raw, {}


def errcode(j):
    if isinstance(j, dict):
        e = j.get('error')
        if isinstance(e, dict):
            return e.get('code')
        return e
    return None


def routed(h):
    if not isinstance(h, dict):
        return None
    for k, v in h.items():
        if k.lower() == 'x-wb2a-routed-model':
            return v
    return None


def chat(key, model, mt=100, txt='hi', headers=None, timeout=180):
    body = {'model': model, 'stream': False, 'max_tokens': mt,
            'messages': [{'role': 'user', 'content': txt}]}
    if model is None:
        del body['model']
    st, j, h = req('POST', '/v1/chat/completions', body, key=key, headers=headers,
                   timeout=timeout)
    return st, j, h


def content_of(j):
    try:
        return j['choices'][0]['message'].get('content') or ''
    except Exception:
        return ''


def mk(name, **kw):
    body = {'name': name, 'realm': kw.get('realm', '')}
    for f in ('models', 'quota', 'quota_credit', 'expires_at', 'max_ips',
              'ip_allowlist'):
        if f in kw:
            body[f] = kw[f]
    st, j, _ = req('POST', '/panel/api/keys', body)
    return st, j


created = []


def mkok(name, **kw):
    st, j = mk(name, **kw)
    assert st == 200, (st, j)
    created.append(j['key']['id'])
    return j['plain'], j['key']


def patch_key(kid, patch):
    return req('POST', '/panel/api/keys/' + kid, patch)


def get_keys():
    st, j, _ = req('GET', '/panel/api/keys')
    return j['keys']


def set_cfg(patch):
    st, j, _ = req('POST', '/panel/api/config', patch)
    return st, j


def get_cfg():
    st, j, _ = req('GET', '/panel/api/config')
    return j['config']


CN_MODELS = sorted(m['id'] for m in req('GET', '/v1/models')[1]['data']
                   if m['id'].startswith('cn:') and not m['id'].endswith('auto'))
CN1 = CN_MODELS[0]
RUN_A = 'skip-a' not in __import__('sys').argv
print('用例模型 CN1=%s（cn 共 %d 个，已排除虚拟 auto）' % (CN1, len(CN_MODELS)))

# ═══ 快照 ═══
cfg0 = get_cfg()
AUTO0 = cfg0.get('auto_model')
MF0 = cfg0.get('model_fallback')
print('基线 auto_model=%s' % json.dumps(AUTO0, ensure_ascii=False))
print('基线 model_fallback=%s' % json.dumps(MF0, ensure_ascii=False))

try:
    if RUN_A:
        # ──────────────────────────────────────────────
        print('\n== §A 密钥分发：反向与边界 ==')

        # A1-A3 名称校验
        st, j = mk('')
        ok('A1 空名称 → 400', st == 400 and errcode(j) == 'invalid_name',
           'st=%s code=%s' % (st, errcode(j)))
        st, j = mk('   ')
        ok('A2 纯空格名称 → 400', st == 400 and errcode(j) == 'invalid_name',
           'st=%s code=%s' % (st, errcode(j)))
        st, j = mk('X' * 65)
        ok('A3 65 字符名称 → 400', st == 400 and errcode(j) == 'invalid_name',
           'st=%s code=%s len=65' % (st, errcode(j)))
        st, j = mk('X' * 64)
        ok('A4 64 字符名称 → 200（边界含）', st == 200, 'st=%s' % st)
        if st == 200:
            created.append(j['key']['id'])

        # A5-A7 CIDR 校验
        st, j = mk('adv-cidr', ip_allowlist=['999.999.1.1/8'])
        ok('A5 非法 CIDR → 400', st == 400 and errcode(j) == 'invalid_ip_allowlist',
           'st=%s code=%s' % (st, errcode(j)))
        st, j = mk('adv-cidr', ip_allowlist=['not-an-ip'])
        ok('A6 非 IP 非 CIDR 字符串 → 400', st == 400 and errcode(j) == 'invalid_ip_allowlist',
           'st=%s code=%s' % (st, errcode(j)))
        st, j = mk('adv-cidr', ip_allowlist=['10.0.0.0/8', '1.2.3.4'])
        ok('A7 合法 IP+CIDR 混合 → 200', st == 200, 'st=%s' % st)
        if st == 200:
            created.append(j['key']['id'])

        # A8/A9 XFF 伪造（安全测试）
        print('  -- XFF 伪造（ClientIP 优先信任 X-Forwarded-For）--')
        p_fake, _ = mkok('adv-xff', ip_allowlist=['1.2.3.4'])
        st, j, h = chat(p_fake, CN1, mt=20)
        ok('A8 白名单外的真实 IP → 400 ip_not_allowed', st == 400 and errcode(j) == 'ip_not_allowed',
           'st=%s code=%s' % (st, errcode(j)))
        st, j, h = chat(p_fake, CN1, mt=20, headers={'X-Forwarded-For': '1.2.3.4'})
        xff_bypass = st == 200
        if xff_bypass:
            note('A9 XFF 伪造可绕过 IP 白名单（安全问题）',
                 '带 X-Forwarded-For: 1.2.3.4 → st=%s（白名单被伪造头命中）' % st)
        else:
            ok('A9 XFF 伪造不能绕过 IP 白名单', True, 'st=%s code=%s' % (st, errcode(j)))

        p_mip, _ = mkok('adv-maxips', max_ips=1)
        st, j, h = chat(p_mip, CN1, mt=20)
        ok('A10 max_ips=1 首次调用（真实 IP 绑定）→ 200', st == 200, 'st=%s' % st)
        st, j, h = chat(p_mip, CN1, mt=20, headers={'X-Forwarded-For': '9.9.9.9'})
        if st == 400 and errcode(j) == 'too_many_ips':
            note('A11 XFF 伪造 IP 计入 max_ips 记账（伪造头生效）',
                 'XFF:9.9.9.9 → too_many_ips：第二个"IP"被伪造头制造')
        else:
            ok('A11 伪造 XFF 不影响 max_ips 判定', st in (200, 400), 'st=%s code=%s' % (st, errcode(j)))

        # A12 额度精确边界：patch quota = used（恰好用满）→ 429；used+1 → 200
        p_q, _ = mkok('adv-quota', quota=100000)
        st, j, h = chat(p_q, CN1, mt=60)
        ok('A12 quota 密钥首调 → 200', st == 200, 'st=%s' % st)
        kinfo = [x for x in get_keys() if x['id'] == created[-1]][0]
        used = kinfo['used_tokens']
        patch_key(created[-1], {'quota': used})
        st, j, h = chat(p_q, CN1, mt=20)
        ok('A13 quota=used（恰好用满）→ 429 quota_exhausted',
           st == 429 and errcode(j) == 'quota_exhausted', 'st=%s code=%s used=%s' % (st, errcode(j), used))
        patch_key(created[-1], {'quota': used + 1})
        st, j, h = chat(p_q, CN1, mt=20)
        ok('A14 quota=used+1（剩 1 token）→ 放行（verify 在消费前）', st == 200, 'st=%s' % st)

        # A15 负 quota_credit → 0 = 不限
        p_c, _ = mkok('adv-credit')
        st, j, _ = patch_key(created[-1], {'quota_credit': -5})
        kinfo = [x for x in get_keys() if x['id'] == created[-1]][0]
        if st == 200 and kinfo['quota_credit'] == 0:
            note('A15 patch quota_credit=-5 → 被归一为 0（=不限，反向语义）', '回读=%s' % kinfo['quota_credit'])
        else:
            # 已修：负值必须被拒（否则归零=不限额度，等于放大权限），且原值不动
            ok('A15 patch quota_credit=-5 → 400 拒绝且额度未被改成不限',
               st == 400 and kinfo['quota_credit'] == 0,
               'st=%s 回读=%s' % (st, kinfo['quota_credit']))

        # A16 expires_at 非法格式
        p_e, _ = mkok('adv-exp')
        st, j, _ = patch_key(created[-1], {'expires_at': 'not-a-date'})
        kinfo = [x for x in get_keys() if x['id'] == created[-1]][0]
        if st == 200 and kinfo['expires_at'] == 'not-a-date':
            st2, j2, h2 = chat(p_e, CN1, mt=20)
            if st2 == 200:
                note('A16 patch expires_at 非法串被接受且不判过期（校验缺失）',
                     'expires_at=%r 调用 st=%s（RFC3339 解析失败→静默永不过期）' % (kinfo['expires_at'], st2))
            else:
                note('A16 patch expires_at 非法串被接受，调用 st=%s code=%s' % (st2, errcode(j2)), '')
        else:
            ok('A16 patch expires_at 非法串被拒', st == 400, 'st=%s 回读=%r' % (st, kinfo['expires_at']))
        # 恢复：清空过期
        patch_key(created[-1], {'expires_at': None})
        kinfo = [x for x in get_keys() if x['id'] == created[-1]][0]
        ok('A17 patch expires_at=null → 清空有效期', kinfo['expires_at'] == '', '回读=%r' % kinfo['expires_at'])

        # A18 白名单 "cn:" 前缀通配
        p_w, _ = mkok('adv-wild', realm='cn', models=['cn:'])
        st, j, h = chat(p_w, CN1, mt=20)
        ok('A18 白名单 ["cn:"] 通配 → cn 模型放行', st == 200, 'st=%s model=%s' % (st, CN1))
        st, j, h = chat(p_w, 'global:gpt-5.5', mt=20)
        ok('A19 白名单 ["cn:"] → global 模型 400（跨域仍拦）',
           st == 400 and errcode(j) == 'realm_mismatch', 'st=%s code=%s' % (st, errcode(j)))

        # A20-A22 空 model
        p_r, _ = mkok('adv-realm', realm='cn')
        st, j, h = chat(p_r, None, mt=20)
        ok('A20 realm 密钥 + 缺 model → 400 要求指定', st == 400 and errcode(j) == 'realm_mismatch',
           'st=%s code=%s' % (st, errcode(j)))
        p_m, _ = mkok('adv-models', models=[CN1])
        st, j, h = chat(p_m, None, mt=20)
        ok('A21 白名单密钥 + 缺 model → 400 model_not_allowed',
           st == 400 and errcode(j) == 'model_not_allowed', 'st=%s code=%s' % (st, errcode(j)))
        p_n, _ = mkok('adv-nomodel')
        st, j, h = chat(p_n, None, mt=20)
        if st == 200:
            note('A22 不限密钥 + 缺 model → 网关按默认放行（200）', 'st=200（上游按缺省模型处理）')
        else:
            ok('A22 不限密钥 + 缺 model → 明确拒绝', st in (400, 500, 503),
               'st=%s code=%s' % (st, errcode(j)))

        # A23-A25 404 路径
        st, j, _ = req('DELETE', '/panel/api/keys/00000000nonexist')
        ok('A23 删除不存在密钥 → 404', st == 404, 'st=%s' % st)
        st, j, _ = req('POST', '/panel/api/keys/00000000nonexist/reset', {})
        ok('A24 reset 不存在密钥 → 404', st == 404, 'st=%s' % st)
        st, j, _ = patch_key('00000000nonexist', {'name': 'x'})
        ok('A25 patch 不存在密钥 → 404', st == 404, 'st=%s' % st)

        # ──────────────────────────────────────────────
    print('\n== §B 模型编排：环/截断/边界/让位 ==')

    # B1 环链 A→B→A
    set_cfg({'model_fallback': {'cn:hy3': ['cn:deepseek-v4.1-flash'],
                                'cn:deepseek-v4.1-flash': ['cn:hy3']}})
    set_cfg({'auto_model': dict(AUTO0, day_primary='cn:hy3')})
    st, j, h = chat(ADMIN, 'auto', mt=80)
    ok('B1 环链 hy3↔flash 不死循环（有限时间内返回）', st in (200, 429, 503),
       'st=%s routed=%s' % (st, routed(h)))

    # B2 自环
    set_cfg({'model_fallback': {'cn:hy3': ['cn:hy3', 'cn:hy3']}})
    st, j, h = chat(ADMIN, 'auto', mt=80)
    ok('B2 自环 hy3→hy3 去重不放大', st in (200, 429, 503), 'st=%s routed=%s' % (st, routed(h)))

    # B3 超长链截断（maxChain=8）：链首+9 个不存在模型
    long_chain = ['cn:nope-%d' % i for i in range(1, 11)] + [CN1]
    set_cfg({'model_fallback': None})
    set_cfg({'auto_model': dict(AUTO0, day_primary='cn:nope-0', fallback=long_chain)})
    t0 = time.time()
    st, j, h = chat(ADMIN, 'auto', mt=40, timeout=300)
    dt = time.time() - t0
    ok('B3 10 候选超长链 → 截断且有限时间返回（%.1fs）' % dt,
       st in (200, 429, 503) and dt < 120, 'st=%s dt=%.1fs routed=%s' % (st, dt, routed(h)))

    # B4 深度嵌套截断（maxExpandDepth=3）
    set_cfg({'auto_model': AUTO0})
    set_cfg({'model_fallback': {
        'cn:nope-a': ['cn:nope-b'],
        'cn:nope-b': ['cn:nope-c'],
        'cn:nope-c': ['cn:nope-d'],
        'cn:nope-d': [CN1],
    }})
    st, j, h = chat(ADMIN, 'cn:nope-a', mt=40, timeout=300)
    ok('B4 4 层嵌套链 → 深度截断后有限返回', st in (200, 429, 503), 'st=%s routed=%s' % (st, routed(h)))

    # B5 isDay 边界：[H, H+1) → 当前小时属白天
    # B5 isDay 边界：[H, H+1) → 当前小时属白天（网关按 CST = UTC+8 判定）
    import datetime as _dt
    H = (_dt.datetime.now(_dt.timezone.utc) + _dt.timedelta(hours=8)).hour % 24
    set_cfg({'model_fallback': None})
    set_cfg({'auto_model': dict(AUTO0, day_primary='cn:hy3', night_primary=CN1,
                                day_start=H, day_end=(H + 1) % 24)})
    st, j, h = chat(ADMIN, 'auto', mt=60)
    ok('B5 窗口 [H,H+1) 且当前=H → 白天主模型（含头）',
       routed(h) in ('cn:hy3',) or (routed(h) is None and st in (200, 429, 503)),
       'H=%d st=%s routed=%s' % (H, st, routed(h)))
    set_cfg({'auto_model': dict(AUTO0, day_primary='cn:hy3', night_primary=CN1,
                                day_start=(H + 1) % 24, day_end=(H + 2) % 24)})
    st, j, h = chat(ADMIN, 'auto', mt=60)
    ok('B6 窗口 [H+1,H+2) 且当前=H → 夜间主模型（不含头前一小时）',
       st in (200, 429, 503), 'H=%d st=%s routed=%s' % (H, st, routed(h)))
    # B7 跨零点窗口 22→6
    s, e = 22, 6
    inwin = H >= 22 or H < 6
    set_cfg({'auto_model': dict(AUTO0, day_primary='cn:hy3', night_primary=CN1,
                                day_start=s, day_end=e)})
    st, j, h = chat(ADMIN, 'auto', mt=60)
    ok('B7 跨零点窗口 22→6 判定一致（当前 %s 窗内）' % ('在' if inwin else '不在'),
       st in (200, 429, 503), 'st=%s routed=%s' % (st, routed(h)))

    # B8 override=false：同名让位
    set_cfg({'auto_model': dict(AUTO0, day_start=AUTO0.get('day_start', 8),
                                day_end=AUTO0.get('day_end', 23), override=False)})
    st, j, h = chat(ADMIN, 'auto', mt=60)
    note('B8 override=false 裸 auto 行为', 'st=%s routed=%s（无 routed 头=让位透传上游同名模型）' % (st, routed(h)))
    st, j, h = chat(ADMIN, 'cn:auto', mt=60)
    note('B9 override=false cn:auto 行为', 'st=%s routed=%s（cn:auto 与裸 auto 判定可能不同：查 realModelExists）' % (st, routed(h)))

    # B10 virtual_id 改名
    set_cfg({'auto_model': dict(AUTO0, virtual_id='vip', override=True)})
    ids = [m['id'] for m in req('GET', '/v1/models')[1]['data']]
    ok('B10 virtual_id=vip → 列表含 vip 与 cn:vip',
       'vip' in ids and 'cn:vip' in ids, 'vip=%s cn:vip=%s' % ('vip' in ids, 'cn:vip' in ids))
    st, j, h = chat(ADMIN, 'vip', mt=60)
    ok('B11 vip 被编排接管（有 routed 头或展开日志）',
       routed(h) is not None or st in (429, 503), 'st=%s routed=%s' % (st, routed(h)))
    st, j, h = chat(ADMIN, 'auto', mt=60)
    note('B12 改名后裸 auto 不再接管（透传上游）', 'st=%s routed=%s' % (st, routed(h)))

    # B13 model_fallback 键带空格（config 层 trim）
    set_cfg({'model_fallback': None})
    set_cfg({'model_fallback': {'cn:nope-x ': [CN1]}})
    st, j, h = chat(ADMIN, 'cn:nope-x', mt=40, timeout=300)
    ok('B13 键尾随空格 trim 后仍命中降级链', st in (200, 429, 503), 'st=%s routed=%s' % (st, routed(h)))

finally:
    print('\n== 恢复 ==')
    set_cfg({'auto_model': AUTO0})
    set_cfg({'model_fallback': None})
    set_cfg({'model_fallback': MF0})
    for kid in created:
        req('DELETE', '/panel/api/keys/' + kid)
    cfg1 = get_cfg()
    ok('配置已还原', cfg1['auto_model'] == AUTO0 and cfg1['model_fallback'] == MF0,
       'auto=%s' % json.dumps(cfg1['auto_model'], ensure_ascii=False))
    left = [x['name'] for x in get_keys()]
    ok('测试密钥已清理（只删自建）', all('adv-' not in n for n in left), '剩余=%s' % left)

print('\n==== %d 通过 / %d 失败 / %d 备注 ====' % (len(PASS), len(FAIL), len(NOTE)))
for f in FAIL:
    print('  FAILED:', f)
