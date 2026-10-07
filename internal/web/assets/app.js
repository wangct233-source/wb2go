/*
 * wb2go 面板脚本
 *
 * 约定：
 *   - 纯原生 JS，无框架无构建，与 HTML/CSS 一起 go:embed 进二进制
 *   - 所有 innerHTML 拼接必须经过 escapeHtml()；能用 textContent 的地方优先用
 *   - 轮询三档：概览 10s（仅可见时）、账号 15s、调度 30s；页面隐藏时全部暂停
 *   - 防重入：每个刷新函数有独立的 busy 标志，避免慢请求叠成风暴
 */

(function () {
  'use strict';

  // ---------- 基础工具 ----------

  var $ = function (id) { return document.getElementById(id); };

  function escapeHtml(v) {
    if (v === null || v === undefined) return '';
    return String(v).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function fmtInt(n) {
    n = Number(n) || 0;
    return n.toLocaleString('en-US');
  }

  // 紧凑格式：超过 1 万才缩写。低于 1 万直接显示完整数字（更精确）。
  function fmtCompact(n) {
    n = Number(n) || 0;
    if (Math.abs(n) >= 1e9) return (n / 1e9).toFixed(1) + 'B';
    if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (Math.abs(n) >= 1e4) return (n / 1e3).toFixed(1) + 'K';
    return fmtInt(n);
  }

  function fmtUptime(sec) {
    sec = Math.max(0, Math.floor(Number(sec) || 0));
    var d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600),
      m = Math.floor(sec % 3600 / 60), s = sec % 60;
    if (d) return d + 'd ' + h + 'h';
    if (h) return h + 'h ' + m + 'm';
    if (m) return m + 'm ' + s + 's';
    return s + 's';
  }

  function fmtContext(n) {
    n = Number(n) || 0;
    if (n >= 1e6) return (n / 1e6).toFixed(n % 1e6 === 0 ? 0 : 1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(n % 1e3 === 0 ? 0 : 1) + 'K';
    return String(n);
  }

  function fmtTime(ts) {
    if (!ts) return '—';
    var d = new Date(ts < 1e12 ? ts * 1000 : ts);
    if (isNaN(d.getTime())) return '—';
    return d.toLocaleString('zh-CN', { hour12: false });
  }

  // 刻度上界取 1/2/5/10 × 10ⁿ，视觉上比直接用最大值更整齐
  function niceCeil(v) {
    if (!v || v <= 0) return 1;
    var exp = Math.floor(Math.log10(v));
    var base = Math.pow(10, exp);
    var f = v / base;
    var m = f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10;
    return m * base;
  }

  // ---------- 网络 ----------

  // requestJSON 统一的取数封装：超时控制 + 不缓存 + 不抛异常。
  // 返回 {ok, status, data}，由调用方决定怎么处理失败。
  function requestJSON(url, options, timeoutMs) {
    timeoutMs = timeoutMs || 45000;
    var ctrl = typeof AbortController !== 'undefined' ? new AbortController() : null;
    var timer = setTimeout(function () { if (ctrl) ctrl.abort(); }, timeoutMs);
    var opts = Object.assign({ cache: 'no-store', headers: {} }, options || {});
    if (ctrl) opts.signal = ctrl.signal;
    return fetch(url, opts).then(function (r) {
      clearTimeout(timer);
      return r.text().then(function (txt) {
        var data = null;
        try { data = txt ? JSON.parse(txt) : null; } catch (e) { data = { error: { message: '服务返回了非 JSON 内容' } }; }
        return { ok: r.ok, status: r.status, data: data };
      });
    }).catch(function (e) {
      clearTimeout(timer);
      var msg = (e && e.name === 'AbortError') ? '请求超时，请检查上游连接后重试' : ('网络错误：' + (e && e.message ? e.message : e));
      return { ok: false, status: 0, data: { error: { message: msg } } };
    });
  }

  function postJSON(url, payload, timeoutMs) {
    return requestJSON(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload || {})
    }, timeoutMs);
  }

  // apiError 从多种错误形态里取出可读文案：
  //   {error:{message}} / {message} / {data:{code,message}} / 裸字符串
  function apiError(data, status) {
    if (!data) return 'HTTP ' + status + '：无响应内容';
    if (data.error) {
      if (typeof data.error === 'string') return data.error;
      if (data.error.message) return data.error.message + (data.error.code ? '（' + data.error.code + '）' : '');
    }
    if (data.message) return data.message;
    if (data.data) {
      var inner = data.data;
      var code = inner.code ? '业务码 ' + inner.code + '：' : '';
      return 'HTTP ' + status + '，' + code + (inner.message || inner.msg || JSON.stringify(inner).slice(0, 160));
    }
    return 'HTTP ' + status + '：' + JSON.stringify(data).slice(0, 200);
  }

  // ---------- UI 反馈 ----------

  var toastTimer = null;
  function toast(title, body, isError) {
    var el = $('toast');
    $('toast-title').textContent = title;
    $('toast-body').textContent = body || '';
    el.className = 'toast visible' + (isError ? ' error' : '');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el.className = 'toast'; }, isError ? 9000 : 3500);
  }

  function setMsg(id, text, isError) {
    var el = $(id);
    if (!el) return;
    if (!text) { el.className = 'msg'; el.textContent = ''; return; }
    el.className = 'msg ' + (isError ? 'msg-err' : 'msg-ok');
    el.textContent = text;
  }

  function setBusy(buttons, on) {
    buttons.forEach(function (b) { if (b) b.classList.toggle('busy', !!on); });
  }

  function setStatePill(id, text, on) {
    var el = $(id);
    if (!el) return;
    el.textContent = text;
    el.className = 'state-pill' + (on ? ' on' : '');
  }

  function statusBadge(st) {
    if (st === '可用') return '<span class="badge badge-ok">可用</span>';
    if (st === '已禁用') return '<span class="badge badge-none">已禁用</span>';
    if (st === '限流冷却' || st === '短冷却') return '<span class="badge badge-cool">' + escapeHtml(st) + '</span>';
    if (st === '积分冷却') return '<span class="badge badge-warn">积分冷却</span>';
    return '<span class="badge badge-warn">' + escapeHtml(st || '—') + '</span>';
  }

  // ---------- 区域（国内版 / 国际版） ----------
  //
  // 两套账号体系完全独立：域名、模型目录、计费、登录入口都不同。
  // 面板各处都要显示/筛选区域，所以这里统一维护一份元数据与读写函数。

  var REALM_META = {
    cn: { name: '国内版', short: 'CN', badge: '🇨🇳 国内版',
          domains: ['copilot.tencent.com', 'www.codebuddy.cn'],
          hasCheckin: true, hasTasks: true },
    intl: { name: '国际版', short: 'INTL', badge: '🌐 国际版',
            domains: ['www.workbuddy.ai'],
            hasCheckin: false, hasTasks: false }
  };

  var currentRealm = 'cn';   // 当前选择的区域（影响登录入口与账号筛选）
  var realmFilter = '';      // 账号列表的区域筛选，'' = 全部

  function realmBadge(key) {
    var m = REALM_META[key] || REALM_META.cn;
    return '<span class="realm-badge ' + escapeHtml(key || 'cn') + '">' + escapeHtml(m.badge) + '</span>';
  }

  // setRealm 切换区域：改配置 + 同步三个 UI 控件。
  // 出口区域是全局设置（决定"没有明确归属的模型走哪区"），会落盘。
  function setRealm(key, persist) {
    if (!REALM_META[key]) return;
    currentRealm = key;
    var top = document.querySelector('input[name=realm-top][value="' + key + '"]');
    var login = document.querySelector('input[name=realm-login][value="' + key + '"]');
    if (top) top.checked = true;
    if (login) login.checked = true;
    renderRealmNote();
    if (persist) {
      postJSON('/panel/api/config', { default_realm: key }).then(function (r) {
        if (!(r.ok && r.data.success)) toast('切换失败', apiError(r.data, r.status), true);
      });
    }
  }

  // renderRealmNote 在登录区下方说明当前区域的能力差异 ——
  // 国际版没有签到与成长任务，事先说清楚比事后困惑好。
  function renderRealmNote() {
    var m = REALM_META[currentRealm];
    var el = $('realm-note');
    if (!el) return;
    el.innerHTML = '域名 <code>' + escapeHtml(m.domains.join(' / ')) + '</code> · ' +
      (m.hasCheckin ? '支持每日签到与成长任务' : '无签到 / 成长任务（国际版走订阅权益）');
  }

  // 顶栏切换：改全局出口
  document.querySelectorAll('input[name=realm-top]').forEach(function (r) {
    r.addEventListener('change', function () { setRealm(this.value, true); });
  });
  // 登录区切换：只改这次登录的区域，不动全局设置
  document.querySelectorAll('input[name=realm-login]').forEach(function (r) {
    r.addEventListener('change', function () { setRealm(this.value, false); });
  });
  // 账号列表筛选
  document.querySelectorAll('input[name=realm-filter]').forEach(function (r) {
    r.addEventListener('change', function () {
      realmFilter = this.value;
      refreshAccounts();
    });
  });

  // ---------- 主题 ----------

  function applyTheme(theme, persist) {
    document.documentElement.setAttribute('data-theme', theme);
    $('theme-toggle').checked = (theme === 'dark');
    if (persist) {
      localStorage.setItem('wb2go-theme', theme);
      // 加临时类让所有元素走一次过渡，360ms 后移除
      document.documentElement.classList.add('theme-anim');
      setTimeout(function () { document.documentElement.classList.remove('theme-anim'); }, 380);
    }
  }

  $('theme-toggle').addEventListener('change', function () {
    applyTheme(this.checked ? 'dark' : 'light', true);
  });

  // ---------- 导航 ----------

  var currentTab = 'overview';

  function switchTab(name) {
    currentTab = name;
    document.querySelectorAll('.nav-item').forEach(function (b) {
      b.classList.toggle('active', b.dataset.tab === name);
    });
    document.querySelectorAll('.tab-page').forEach(function (p) {
      p.classList.toggle('active', p.dataset.page === name);
    });
    var btn = document.querySelector('.nav-item[data-tab="' + name + '"]');
    $('page-title').textContent = btn ? btn.dataset.title : '';
    history.replaceState(null, '', '#' + name);
    // 切到概览时立即刷新一次，不等轮询
    if (name === 'overview') refreshOverview();
    if (name === 'accounts') refreshAccounts();
    if (name === 'usage') refreshUsage();
    if (name === 'scheduler') refreshScheduler();
    if (name === 'models') refreshModels();
  }

  document.querySelectorAll('.nav-item').forEach(function (b) {
    b.addEventListener('click', function () { switchTab(this.dataset.tab); });
  });

  // ---------- 概览 ----------

  var ovData = null, ovRange = 14, ovDay = '', ovBusy = false;

  function refreshOverview() {
    if (ovBusy || document.hidden) return;
    ovBusy = true;
    requestJSON('/panel/api/overview?days=30', null, 15000).then(function (res) {
      ovBusy = false;
      if (!res.ok) { setMsg('ov-msg', apiError(res.data, res.status), true); return; }
      setMsg('ov-msg', '');
      ovData = res.data;
      renderOverview();
      renderChips(res.data.service || {}, res.data.accounts || {});
    });
  }

  function renderChips(svc, acc) {
    $('chip-service').innerHTML = '<span class="chip-key">服务</span><code>' +
      escapeHtml(svc.service || 'wb2go') + ' v' + escapeHtml(svc.version || '?') + '</code>';
    $('chip-accounts').innerHTML = '<span class="chip-key">账号</span><code>' +
      escapeHtml(acc.healthy != null ? acc.healthy + '/' + acc.total : '—') + '</code>';
    $('chip-region').innerHTML = '<span class="chip-key">出口</span><code>' +
      escapeHtml(svc.default_realm || '—') + '</code>';
    $('chip-identity').innerHTML = '<span class="chip-key">身份</span><code>' +
      escapeHtml(svc.identity || '—') + '</code>';
    $('foot-version').textContent = 'v' + (svc.version || '?');
  }

  function setStat(id, value, title) {
    var el = $(id);
    if (!el) return;
    el.textContent = value;
    if (title) el.title = title;
  }

  function renderOverview() {
    if (!ovData) return;
    var u = ovData.usage || {}, t = u.totals || {}, today = u.today || {};

    setStat('ov-total', fmtCompact(t.total_tokens), fmtInt(t.total_tokens) + ' tokens');
    setStat('ov-total-sub', fmtInt(t.requests) + ' 次请求');
    setStat('ov-input', fmtCompact(t.prompt_tokens - (t.cached_tokens || 0)), fmtInt(t.prompt_tokens) + ' 含缓存');
    setStat('ov-input-sub', '缓存 ' + fmtCompact(t.cached_tokens || 0));
    setStat('ov-output', fmtCompact(t.completion_tokens), fmtInt(t.completion_tokens));
    setStat('ov-output-sub', t.failed ? ('失败 ' + fmtInt(t.failed)) : '全部成功');
    setStat('ov-cache', fmtCompact(t.cached_tokens || 0), fmtInt(t.cached_tokens || 0));
    setStat('ov-cache-sub', '命中上游前缀缓存');
    setStat('ov-today', fmtCompact(today.total_tokens), fmtInt(today.total_tokens));
    setStat('ov-today-sub', (today.date || '') + ' · ' + fmtInt(today.requests) + ' 次');

    var acc = ovData.accounts || {}, svc = ovData.service || {};
    setStat('ov-accounts', (acc.healthy != null ? acc.healthy : '--') + ' / ' + (acc.total != null ? acc.total : '--'),
      '可用 / 总数');
    setStat('ov-accounts-sub', '积分 ' + fmtCompact(acc.credits_remaining || 0));

    $('ov-version').textContent = svc.version || '—';
    $('ov-uptime').textContent = fmtUptime(svc.uptime_seconds);
    $('ov-healthy').textContent = (acc.healthy != null ? acc.healthy : '—') + ' / ' + (acc.total != null ? acc.total : '—');
    $('ov-inflight').textContent = svc.in_flight != null ? String(svc.in_flight) : '—';
    $('ov-sticky').textContent = svc.sticky_binds != null ? String(svc.sticky_binds) : '—';
    $('ov-region').textContent = svc.default_realm || '—';
    $('ov-identity').textContent = svc.identity || '—';
    var rm = svc.realms || {};
    $('ov-realms').textContent = '国内 ' + (rm.cn || 0) + ' · 国际 ' + (rm.intl || 0);
    $('ov-waf').innerHTML = svc.waf_blocked
      ? '<span class="badge badge-warn">已拦截（60s）</span>'
      : '<span class="badge badge-ok">正常</span>';

    renderChart();
    renderDaysTable();
  }

  function renderChart() {
    if (!ovData) return;
    var rows = ((ovData.usage || {}).daily || []).slice(0, ovRange).reverse();
    var chart = $('ov-chart'), axis = $('ov-axis');
    var scale = niceCeil(Math.max.apply(null, rows.map(function (r) {
      return (r.input_tokens || 0) + (r.output_tokens || 0);
    }).concat([1])));

    $('ov-y-max').textContent = fmtCompact(scale);
    $('ov-y-mid').textContent = fmtCompact(Math.round(scale / 2));

    // 数量不变时复用 DOM 节点，只改高度；变了才重建
    var rebuild = chart.children.length !== rows.length;
    if (rebuild) {
      chart.innerHTML = rows.map(function (r) {
        return '<div class="bar-col" tabindex="0" role="button" data-day="' + escapeHtml(r.date) +
          '" aria-label="' + escapeHtml(r.date + ' 总计 ' + r.total_tokens + ' tokens') +
          '"><div class="bar-stack">' +
          '<span class="bar-seg seg-out"></span>' +
          '<span class="bar-seg seg-in"></span>' +
          '<span class="bar-seg seg-cache"></span>' +
          '</div></div>';
      }).join('');
      axis.innerHTML = rows.map(function (r) { return '<span></span>'; }).join('');
    }
    var apply = function () {
      rows.forEach(function (r, i) {
        var col = chart.children[i];
        if (!col) return;
        var segs = col.querySelectorAll('.bar-seg');
        var cache = Math.min(r.cached_tokens || 0, r.input_tokens || 0);
        segs[0].style.height = ((r.output_tokens || 0) / scale * 100) + '%';
        segs[1].style.height = (((r.input_tokens || 0) - cache) / scale * 100) + '%';
        segs[2].style.height = (cache / scale * 100) + '%';
      });
      // X 轴稀疏标签：超过 8 个就隔开显示，否则会挤成一团
      var step = Math.max(1, Math.ceil(rows.length / 8));
      axis.innerHTML = rows.map(function (r, i) {
        return '<span>' + (i % step === 0 ? escapeHtml(String(r.date).slice(5)) : '') + '</span>';
      }).join('');
    };
    if (rebuild) {
      // 双 rAF：先让节点进 DOM，再触发布局，才能看到 height 从 0 长出来
      requestAnimationFrame(function () { requestAnimationFrame(apply); });
    } else {
      apply();
    }
    showDay(rows.length ? rows[rows.length - 1].date : '');
  }

  function showDay(day) {
    var rows = ((ovData.usage || {}).daily || []);
    var r = null;
    for (var i = 0; i < rows.length; i++) { if (rows[i].date === day) { r = rows[i]; break; } }
    var box = $('ov-readout');
    if (!r) {
      box.innerHTML = '<span class="readout-date">选择某天查看明细</span>';
      return;
    }
    box.innerHTML =
      '<span class="readout-date">' + escapeHtml(r.date) + '</span>' +
      '<span class="readout-item"><span class="swatch sw-in"></span>输入 <strong>' + fmtInt(r.input_tokens) + '</strong></span>' +
      '<span class="readout-item"><span class="swatch sw-cache"></span>缓存 <strong>' + fmtInt(r.cached_tokens || 0) + '</strong></span>' +
      '<span class="readout-item"><span class="swatch sw-out"></span>输出 <strong>' + fmtInt(r.output_tokens) + '</strong></span>' +
      '<span>请求 <strong>' + fmtInt(r.requests) + '</strong>（失败 ' + fmtInt(r.failed || 0) + '）</span>';
  }

  function selectDay(day) {
    ovDay = day;
    document.querySelectorAll('#ov-chart .bar-col').forEach(function (c) {
      c.classList.toggle('sel', c.dataset.day === day);
    });
    document.querySelectorAll('#ov-days-body tr').forEach(function (tr) {
      var on = tr.dataset.day === day;
      tr.classList.toggle('sel', on);
      if (on) tr.style.boxShadow = 'inset 3px 0 var(--edge)';
      else tr.style.boxShadow = '';
      if (on) tr.scrollIntoView({ block: 'nearest' });
    });
    showDay(day);
  }

  $('ov-chart').addEventListener('mouseover', function (e) {
    var col = e.target.closest('.bar-col');
    if (col && !ovDay) showDay(col.dataset.day);
  });
  $('ov-chart').addEventListener('click', function (e) {
    var col = e.target.closest('.bar-col');
    if (col) selectDay(col.dataset.day);
  });
  $('ov-chart').addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    var col = e.target.closest('.bar-col');
    if (col) { e.preventDefault(); selectDay(col.dataset.day); }
  });

  function renderDaysTable() {
    var rows = ((ovData.usage || {}).daily || []).slice();
    if ($('ov-hide-empty').checked) rows = rows.filter(function (r) { return r.requests > 0; });
    var body = $('ov-days-body');
    if (!rows.length) {
      body.innerHTML = '<tr><td colspan="8" style="text-align:center;color:var(--faint)">暂无数据</td></tr>';
      return;
    }
    body.innerHTML = rows.map(function (r) {
      var cls = r.requests ? '' : 'row-muted';
      var today = r.date === ((ovData.usage || {}).today || {}).date ? ' <span class="badge badge-active">今日</span>' : '';
      return '<tr data-day="' + escapeHtml(r.date) + '" class="' + cls + '" style="cursor:pointer">' +
        '<td>' + escapeHtml(r.date) + today + '</td>' +
        '<td class="num">' + fmtInt(r.requests) + '</td>' +
        '<td class="num">' + fmtInt(r.failed || 0) + '</td>' +
        '<td class="num">' + fmtInt(r.input_tokens) + '</td>' +
        '<td class="num">' + fmtInt(r.output_tokens) + '</td>' +
        '<td class="num">' + fmtInt(r.cached_tokens || 0) + '</td>' +
        '<td class="num">' + fmtInt(r.total_tokens) + '</td>' +
        '<td class="num">' + (r.credits ? Number(r.credits).toFixed(2) : '0.00') + '</td>' +
        '</tr>';
    }).join('');
  }
  $('ov-days-body').addEventListener('click', function (e) {
    var tr = e.target.closest('tr[data-day]');
    if (tr) selectDay(tr.dataset.day);
  });
  $('ov-hide-empty').addEventListener('change', renderDaysTable);
  document.querySelectorAll('input[name=ov-range]').forEach(function (r) {
    r.addEventListener('change', function () {
      ovRange = Number(this.value);
      renderChart();
      renderDaysTable();
    });
  });

  // ---------- 账号 ----------

  var acctBusy = false;

  function refreshAccounts() {
    if (acctBusy) return;
    acctBusy = true;
    requestJSON('/panel/api/accounts', null, 15000).then(function (res) {
      acctBusy = false;
      if (!res.ok) { setMsg('acct-msg', apiError(res.data, res.status), true); return; }
      setMsg('acct-msg', '');
      renderAccounts(res.data);
    });
  }

  function renderAccounts(data) {
    var list = data.accounts || [];
    $('acct-empty').hidden = list.length > 0;
    var healthy = 0, credits = 0;
    list.forEach(function (a) {
      if (a.status === '可用' && !a.disabled) healthy++;
      if (a.credits) credits += a.credits;
    });

    $('acct-total').textContent = list.length;
    $('acct-total-sub').textContent = '含 ' + fmtCompact(data.sticky_binds || 0) + ' 粘性会话';
    $('acct-healthy').textContent = healthy;
    $('acct-healthy-sub').textContent = '当前可接单';
    $('acct-credits').textContent = fmtCompact(credits);
    $('acct-credits-sub').textContent = '剩余积分合计';
    $('acct-sticky').textContent = data.sticky_binds || 0;
    $('acct-sticky-sub').textContent = '按会话绑定账号';

    if (realmFilter) {
      list = list.filter(function (a) { return a.realm === realmFilter; });
    }
    var cn = list.filter(function (a) { return a.realm !== 'intl'; }).length;
    var intl = list.length - cn;
    $('acct-total-sub').textContent = '国内 ' + cn + ' · 国际 ' + intl;

    $('acct-body').innerHTML = list.map(function (a) {
      var uid = escapeHtml(a.uid);
      var cool = a.cool_remaining > 0 ? fmtDuration(a.cool_remaining) : '—';
      var idOpts = ['workbuddy', 'vscode', 'cli'].map(function (v) {
        return '<option value="' + v + '"' + (a.identity === v ? ' selected' : '') + '>' +
          ({ workbuddy: '桌面端', vscode: 'VSCode', cli: 'CLI' })[v] + '</option>';
      }).join('');
      return '<tr data-uid="' + uid + '"' + (a.disabled ? ' class="row-failed"' : '') + '>' +
        '<td><div style="font-weight:600">' + escapeHtml(a.label || uid) + '</div>' +
        '<small class="row-subtitle mono">' + uid + '</small></td>' +
        '<td>' + realmBadge(a.realm) + '</td>' +
        '<td>' + statusBadge(a.status) + (a.disabled_reason ? ' <small class="row-subtitle">' + escapeHtml(a.disabled_reason) + '</small>' : '') + '</td>' +
        '<td class="num">' + (a.credits_known === false ? '—' : fmtCompact(a.credits || 0)) + '</td>' +
        '<td>' + cool + '</td>' +
        '<td><select class="inline-field" data-act="identity" style="height:26px">' + idOpts + '</select></td>' +
        '<td class="num">' + (a.in_flight || 0) + '</td>' +
        '<td>' +
        '<div class="toolbar" style="flex-wrap:nowrap">' +
        '<button class="icon-btn" data-act="checkin" title="签到" aria-label="签到">' +
        '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="4" width="18" height="18" rx="2"/><path d="M16 2v4M8 2v4M3 10h18"/></svg></button>' +
        '<button class="icon-btn" data-act="credits" title="查积分" aria-label="查积分">' +
        '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="9"/><path d="M12 7v10M9 10h6"/></svg></button>' +
        '<button class="icon-btn" data-act="tasks" title="成长任务" aria-label="成长任务">' +
        '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M9 11l3 3L22 4"/><path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/></svg></button>' +
        '<button class="icon-btn" data-act="refresh" title="刷新凭证" aria-label="刷新凭证">' +
        '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12a9 9 0 1 1-3-6.7"/><path d="M21 3v6h-6"/></svg></button>' +
        '<button class="icon-btn" data-act="toggle" title="' + (a.disabled ? '启用' : '停用') + '" aria-label="切换启用">' +
        (a.disabled
          ? '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 6L6 18M6 6l12 12"/></svg>'
          : '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>') +
        '</button>' +
        '<button class="icon-btn danger" data-act="delete" title="移除" aria-label="移除">' +
        '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6"/></svg></button>' +
        '</div></td></tr>';
    }).join('');
  }

  function fmtDuration(sec) {
    sec = Math.max(0, Math.floor(Number(sec) || 0));
    if (sec < 60) return sec + ' 秒';
    if (sec < 3600) return Math.floor(sec / 60) + ' 分';
    return Math.floor(sec / 3600) + ' 时 ' + Math.floor(sec % 3600 / 60) + ' 分';
  }

  // 账号行操作（事件委托）
  $('acct-body').addEventListener('click', function (e) {
    var btn = e.target.closest('button[data-act]');
    if (!btn) return;
    var tr = btn.closest('tr[data-uid]');
    if (!tr) return;
    var uid = tr.dataset.uid;
    var act = btn.dataset.act;

    if (act === 'delete') {
      if (!confirm('确定移除账号 ' + uid + ' ？\n凭证文件会被删除，此操作不可撤销。')) return;
      postJSON('/panel/api/accounts/delete', { uid: uid }).then(function (r) {
        if (r.ok && r.data.success) { toast('已移除', uid); refreshAccounts(); }
        else toast('移除失败', apiError(r.data, r.status), true);
      });
      return;
    }
    if (act === 'toggle') {
      postJSON('/panel/api/accounts/toggle', { uid: uid }).then(function (r) {
        if (r.ok && r.data.success) { toast('已切换', uid); refreshAccounts(); }
        else toast('操作失败', apiError(r.data, r.status), true);
      });
      return;
    }
    var map = { checkin: 'checkin', credits: 'credits', tasks: 'tasks', refresh: 'refresh' };
    var endpoint = '/panel/api/accounts/' + (map[act] || act);
    btn.classList.add('busy');
    postJSON(endpoint, { uid: uid }, 120000).then(function (r) {
      btn.classList.remove('busy');
      if (r.ok && r.data.success) {
        toast('完成', uid + '：' + (r.data.message || 'ok'));
        refreshAccounts();
      } else {
        toast('失败', apiError(r.data, r.status), true);
      }
    });
  });

  $('acct-body').addEventListener('change', function (e) {
    var sel = e.target.closest('select[data-act="identity"]');
    if (!sel) return;
    var tr = sel.closest('tr[data-uid]');
    postJSON('/panel/api/accounts/identity', { uid: tr.dataset.uid, identity: sel.value }).then(function (r) {
      if (r.ok && r.data.success) toast('已切换身份', sel.value);
      else toast('切换失败', apiError(r.data, r.status), true);
    });
  });

  // 批量操作：统一走调度器的同一套逻辑，保证与定时执行行为一致
  function batch(name, confirmText) {
    if (confirmText && !confirm(confirmText)) return;
    var btn = { checkin: $('btn-checkin-all'), credits: $('btn-credits-all'), tasks: $('btn-tasks-all'), travel: $('btn-travel-all'), keepalive: $('btn-keepalive-all') }[name];
    setBusy([btn, $('acct-busy')], true);
    $('acct-busy').classList.add('visible');
    postJSON('/panel/api/batch/' + name, {}, 900000).then(function (r) {
      setBusy([btn, $('acct-busy')], false);
      $('acct-busy').classList.remove('visible');
      if (r.ok && r.data.success) {
        toast('批量' + name + '完成', r.data.message || '');
        setMsg('acct-msg', r.data.detail || r.data.message || '', false);
      } else {
        setMsg('acct-msg', apiError(r.data, r.status), true);
      }
      refreshAccounts();
    });
  }

  $('btn-checkin-all').addEventListener('click', function () { batch('checkin', '将对所有可用账号执行签到，继续？'); });
  $('btn-credits-all').addEventListener('click', function () { batch('credits'); });
  $('btn-tasks-all').addEventListener('click', function () { batch('tasks', '将对所有国内账号执行成长任务，继续？'); });
  $('btn-travel-all').addEventListener('click', function () { batch('travel'); });
  $('btn-keepalive-all').addEventListener('click', function () { batch('keepalive'); });
  $('btn-refresh-all').addEventListener('click', refreshAccounts);

  // ---------- OAuth 登录 ----------

  var oauthTimer = null;

  $('btn-oauth').addEventListener('click', function () {
    setMsg('oauth-msg', '');
    setStatePill('oauth-state', '生成中', false);
    postJSON('/panel/api/oauth/start', { realm: currentRealm }, 30000).then(function (r) {
      if (!r.ok || !r.data.success) {
        setStatePill('oauth-state', '失败', false);
        setMsg('oauth-msg', apiError(r.data, r.status), true);
        return;
      }
      var url = r.data.auth_url || '';
      $('oauth-url').textContent = url ? url.slice(0, 60) + '…' : '—';
      $('oauth-loading').hidden = false;
      setStatePill('oauth-state', '等待授权', true);
      setMsg('oauth-msg', '授权链接已生成，正在打开浏览器…');
      if (url) window.open(url, '_blank');
      startOAuthPoll();
    });
  });

  function startOAuthPoll() {
    if (oauthTimer) clearInterval(oauthTimer);
    oauthTimer = setInterval(function () {
      requestJSON('/panel/api/oauth/status', null, 15000).then(function (r) {
        if (!r.ok) return;
        $('oauth-pending').textContent = r.data.pending || 0;
        if (r.data.account) {
          clearInterval(oauthTimer);
          oauthTimer = null;
          $('oauth-loading').hidden = true;
          setStatePill('oauth-state', '已完成', true);
          var rm = REALM_META[r.data.realm] || REALM_META.cn;
          setMsg('oauth-msg', r.data.account + '（' + rm.name + '）已加入池中，无需重启。', false);
          toast('登录成功', r.data.account);
          refreshAccounts();
        } else if (r.data.expired) {
          clearInterval(oauthTimer);
          oauthTimer = null;
          $('oauth-loading').hidden = true;
          setStatePill('oauth-state', '已超时', false);
          setMsg('oauth-msg', '授权链接已过期，请重新生成。', true);
        }
      });
    }, 3000);
  }

  $('btn-manual').addEventListener('click', function () {
    var payload = {
      uid: $('m-uid').value.trim(),
      accessToken: $('m-token').value.trim(),
      refreshToken: $('m-refresh').value.trim(),
      realm: $('m-realm').value
    };
    if (!payload.uid || !payload.accessToken) {
      setMsg('manual-msg', 'UID 与 Access Token 为必填项。', true);
      return;
    }
    postJSON('/panel/api/accounts/manual', payload, 30000).then(function (r) {
      if (r.ok && r.data.success) {
        setMsg('manual-msg', '已保存并加载。', false);
        $('m-uid').value = $('m-token').value = $('m-refresh').value = '';
        refreshAccounts();
      } else {
        setMsg('manual-msg', apiError(r.data, r.status), true);
      }
    });
  });

  // ---------- 模型 ----------

  function refreshModels() {
    // 走面板自己的端点：/v1/models 需要 API Key，浏览器 fetch 不带凭证会 401
    requestJSON('/panel/api/models', null, 20000).then(function (r) {
      if (!r.ok) { setMsg('models-msg', apiError(r.data, r.status), true); return; }
      setMsg('models-msg', '');
      var list = (r.data && r.data.data) || [];
      $('models-body').innerHTML = list.map(function (m) {
        var caps = [];
        if (m.supports_vision) caps.push('视觉');
        if (m.supports_tools) caps.push('工具');
        if (m.supports_thinking) caps.push('思考');
        if (m.is_default) caps.push('默认');
        return '<tr>' +
          '<td class="mono">' + escapeHtml(m.id) + '</td>' +
          '<td>' + escapeHtml(m.display_name || '—') + '</td>' +
          '<td><span class="badge badge-none">' + escapeHtml(m.realm || '—') + '</span></td>' +
          '<td class="num">' + fmtContext(m.context_length) + '</td>' +
          '<td class="num">' + fmtContext(m.max_output_tokens) + '</td>' +
          '<td>' + (caps.length ? caps.map(function (c) { return '<span class="badge badge-active">' + escapeHtml(c) + '</span>'; }).join(' ') : '—') + '</td>' +
          '<td class="num">' + (m.credits_rate ? Number(m.credits_rate).toFixed(2) + 'x' : '<span class="badge badge-ok">免费</span>') + '</td>' +
          '</tr>';
      }).join('');
    });
  }
  $('btn-models-refresh').addEventListener('click', refreshModels);

  var PRESET_MODELS = ['glm-5.2', 'deepseek-v4.1-flash', 'hy4-preview-f', 'kimi-k3-1', 'gpt-6-astra', 'gemini-3.5-flash'];

  $('btn-test-preset').addEventListener('click', function () {
    $('t-model').value = PRESET_MODELS.join(',');
    setMsg('test-msg', '已填入 ' + PRESET_MODELS.length + ' 个常用模型，点「开始测试」逐个探测。', false);
  });

  $('btn-test').addEventListener('click', function () {
    var models = $('t-model').value.split(/[,，\s]+/).filter(Boolean);
    if (!models.length) { setMsg('test-msg', '请至少填写一个模型 ID。', true); return; }
    var effort = $('t-effort').value;
    var timeout = Number($('t-timeout').value) || 90;
    $('test-table-wrap').hidden = false;
    $('test-body').innerHTML = models.map(function (m) {
      return '<tr data-model="' + escapeHtml(m) + '"><td class="mono">' + escapeHtml(m) +
        '</td><td><span class="badge badge-none">排队中</span></td><td class="num">—</td><td>—</td></tr>';
    }).join('');
    setMsg('test-msg', '开始测试 ' + models.length + ' 个模型…');

    // 串行探测：并发打上游容易被判为异常流量
    var idx = 0;
    function next() {
      if (idx >= models.length) {
        setMsg('test-msg', '测试完成。', false);
        $('test-summary').textContent = '已完成 ' + models.length + ' 个';
        return;
      }
      var model = models[idx++];
      var started = Date.now();
      postJSON('/panel/api/model-test', { model: model, effort: effort, timeout: timeout }, (timeout + 30) * 1000)
        .then(function (r) {
          var tr = $('test-body').querySelector('tr[data-model="' + CSS.escape(model) + '"]');
          if (!tr) return;
          var elapsed = ((Date.now() - started) / 1000).toFixed(1);
          if (r.ok && r.data.success) {
            tr.innerHTML = '<td class="mono">' + escapeHtml(model) + '</td>' +
              '<td><span class="badge badge-ok">通过</span></td>' +
              '<td class="num">' + elapsed + 's</td>' +
              '<td>' + escapeHtml((r.data.reply || '').slice(0, 120) || 'ok') +
              (r.data.usage ? ' · ' + r.data.usage.total_tokens + ' tok' : '') + '</td>';
          } else {
            tr.innerHTML = '<td class="mono">' + escapeHtml(model) + '</td>' +
              '<td><span class="badge badge-warn">失败</span></td>' +
              '<td class="num">' + elapsed + 's</td>' +
              '<td>' + escapeHtml((r.data && r.data.error) || apiError(r.data, r.status)) + '</td>';
          }
          next();
        });
    }
    next();
  });

  // ---------- 用量 ----------

  var usageBusy = false;

  function refreshUsage() {
    if (usageBusy) return;
    usageBusy = true;
    requestJSON('/panel/api/usage/recent?limit=200', null, 20000).then(function (r) {
      usageBusy = false;
      if (!r.ok) { setMsg('usage-msg', apiError(r.data, r.status), true); return; }
      setMsg('usage-msg', '');
      var list = r.data.records || [];
      $('usage-empty').hidden = list.length > 0;
      $('usage-body').innerHTML = list.map(function (x) {
        var badge = x.outcome === 'completed'
          ? '<span class="badge badge-ok">成功</span>'
          : (x.outcome === 'aborted' ? '<span class="badge badge-warn">中断</span>' : '<span class="badge badge-none">失败</span>');
        return '<tr>' +
          '<td>' + fmtTime(x.ts) + '</td>' +
          '<td class="mono">' + escapeHtml(x.model || '—') + '</td>' +
          '<td class="mono">' + escapeHtml((x.uid || '').slice(0, 8) || '—') + '</td>' +
          '<td><span class="badge badge-none">' + escapeHtml(x.realm || '—') + '</span></td>' +
          '<td class="num">' + fmtInt(x.tot || 0) + '</td>' +
          '<td class="num">' + (x.dur ? (x.dur / 1000).toFixed(1) + 's' : '—') + '</td>' +
          '<td>' + badge + (x.errkind ? ' <small class="row-subtitle">' + escapeHtml(x.errkind) + '</small>' : '') + '</td>' +
          '<td class="mono" style="font-size:11px">' + escapeHtml((x.rid || '').slice(-10) || '—') + '</td>' +
          '</tr>';
      }).join('');
    });
  }

  // ---------- 调度 ----------

  var SCHED_TASKS = [
    { key: 'checkin', desc: '每日签到领积分（国内版）' },
    { key: 'travel', desc: '猫猫旅行：自动派出与领奖' },
    { key: 'tasks', desc: '成长任务一键完成并领奖' },
    { key: 'active', desc: '对话活跃上报（点亮连登）' },
    { key: 'keepalive', desc: 'Token 定时保活' },
    { key: 'credits', desc: '批量刷新积分与解冻' },
    { key: 'nightcat', desc: '夜猫子补足对话' }
  ];

  function refreshScheduler() {
    requestJSON('/panel/api/scheduler', null, 15000).then(function (r) {
      if (!r.ok) { setMsg('sched-msg', apiError(r.data, r.status), true); return; }
      setMsg('sched-msg', '');
      var d = r.data;
      $('sched-server-time').textContent = d.server_time + ' · ' + d.timezone;
      $('sched-body').innerHTML = SCHED_TASKS.map(function (t) {
        var running = (d.running || {})[t.key];
        var stamp = (d.last_run || {})[t.key];
        var when = stamp ? stamp % 100 + ':00' : '—';
        return '<tr>' +
          '<td class="mono">' + escapeHtml(t.key) + '</td>' +
          '<td>' + escapeHtml(t.desc) + '</td>' +
          '<td class="num">' + when + '</td>' +
          '<td>' + (running ? '<span class="badge badge-active">运行中</span>' : '<span class="badge badge-none">空闲</span>') + '</td>' +
          '<td><button class="btn btn-secondary btn-sm" data-sched="' + escapeHtml(t.key) + '">立即执行</button></td>' +
          '</tr>';
      }).join('');
    });
  }

  $('sched-body').addEventListener('click', function (e) {
    var btn = e.target.closest('button[data-sched]');
    if (!btn) return;
    var key = btn.dataset.sched;
    if (!confirm('立即执行「' + key + '」？')) return;
    btn.classList.add('busy');
    postJSON('/panel/api/scheduler/trigger', { name: key }, 30000).then(function (r) {
      btn.classList.remove('busy');
      if (r.ok && r.data.success) toast('已触发', key + ' 正在后台执行');
      else toast('触发失败', apiError(r.data, r.status), true);
      setTimeout(refreshScheduler, 2000);
    });
  });

  // ---------- 设置 ----------

  function loadSettings() {
    requestJSON('/panel/api/config', null, 15000).then(function (r) {
      if (!r.ok) { setMsg('save-msg', apiError(r.data, r.status), true); return; }
      var c = r.data.config || {};
      $('s-apikey').value = c.api_key || '';
      $('s-prompt').value = c.system_prompt || '';
      $('s-identity').value = c.identity || 'workbuddy';
      $('s-realm').value = c.default_realm || 'cn';
      setRealm(c.default_realm || 'cn', false);
      $('s-inflight').value = c.max_in_flight || 0;
      $('s-breaker').value = c.breaker_threshold || 0;
      $('s-floor').value = c.credit_floor || 0;
      $('s-reserve').value = c.reserve_credits || 0;
      $('s-daily').value = c.daily_token_limit || 0;
      $('s-idle').value = c.chat_timeout || 300;
      $('s-sanitize').checked = !!c.sanitize;
      $('s-sticky').checked = !!c.sticky_enabled;
      $('s-cachekey').checked = !!c.prompt_cache_key;
      $('s-checkin').checked = !!(c.schedule && c.schedule.checkin_enabled);
      $('s-travel').checked = !!(c.schedule && c.schedule.travel_enabled);
      $('s-tasks').checked = !!(c.schedule && c.schedule.task_enabled);
      $('s-keepalive').checked = !!(c.schedule && c.schedule.keepalive_enabled);
      $('s-balance').checked = !!(c.schedule && c.schedule.balance_enabled);
      // 仓库地址由服务端写死返回，前端只读展示
      $('s-repo').value = (c.update && c.update.github_release) || 'wangct233-source/wb2go';
      $('s-hotupdate').checked = !!(c.update && c.update.enabled);
      $('s-autorestart').checked = !!(c.update && c.update.auto_restart);
      $('version-note').textContent = '当前版本 ' + (r.data.version || '—');
    });
  }

  function num(id) { return Number($(id).value) || 0; }

  $('btn-save').addEventListener('click', function () {
    var payload = {
      api_key: $('s-apikey').value.trim(),
      system_prompt: $('s-prompt').value,
      identity: $('s-identity').value,
      default_realm: $('s-realm').value === 'intl' ? 'intl' : 'cn',
      max_in_flight: num('s-inflight'),
      breaker_threshold: num('s-breaker'),
      credit_floor: num('s-floor'),
      reserve_credits: num('s-reserve'),
      daily_token_limit: num('s-daily'),
      chat_timeout: num('s-idle'),
      sanitize: $('s-sanitize').checked,
      sticky_enabled: $('s-sticky').checked,
      prompt_cache_key: $('s-cachekey').checked,
      schedule: {
        checkin_enabled: $('s-checkin').checked,
        travel_enabled: $('s-travel').checked,
        task_enabled: $('s-tasks').checked,
        keepalive_enabled: $('s-keepalive').checked,
        balance_enabled: $('s-balance').checked
      },
      update: {
        // github_release 由服务端锁定为官方仓库，此处不提交
        enabled: $('s-hotupdate').checked,
        auto_restart: $('s-autorestart').checked
      }
    };
    setBusy([$('btn-save')], true);
    setMsg('save-msg', '');
    postJSON('/panel/api/config', payload, 30000).then(function (r) {
      setBusy([$('btn-save')], false);
      if (r.ok && r.data.success) {
        setMsg('save-msg', r.data.message || '已保存并立即生效。', false);
        toast('已保存', '配置已热生效');
      } else {
        setMsg('save-msg', apiError(r.data, r.status), true);
      }
    });
  });
  $('btn-revert').addEventListener('click', function () { loadSettings(); setMsg('save-msg', '已重新载入当前生效的配置。', false); });

  $('btn-check-update').addEventListener('click', function () {
    setBusy([$('btn-check-update')], true);
    setMsg('update-msg', '');
    postJSON('/panel/api/update/check', {}, 60000).then(function (r) {
      setBusy([$('btn-check-update')], false);
      if (!r.ok || !r.data.success) { setMsg('update-msg', apiError(r.data, r.status), true); return; }
      var d = r.data;
      setMsg('update-msg', d.up_to_date
        ? ('当前 v' + d.current + ' 已是最新版本。')
        : ('发现新版本 v' + d.latest + '（当前 v' + d.current + '）。点「立即更新并重启」完成升级。'), false);
    });
  });

  $('btn-do-update').addEventListener('click', function () {
    if (!confirm('将拉取最新发布版本并替换当前二进制，进程会重启。继续？')) return;
    setBusy([$('btn-do-update')], true);
    setMsg('update-msg', '正在下载并校验…');
    postJSON('/panel/api/update/apply', {}, 180000).then(function (r) {
      setBusy([$('btn-do-update')], false);
      if (r.ok && r.data.success) setMsg('update-msg', r.data.message || '更新完成，进程即将重启。', false);
      else setMsg('update-msg', apiError(r.data, r.status), true);
    });
  });

  // ---------- 轮询 ----------

  function startPolling() {
    // 概览 10s：页面隐藏时跳过（省流量，也避免后台堆积）
    setInterval(function () {
      if (!document.hidden && currentTab === 'overview') refreshOverview();
    }, 10000);
    setInterval(function () {
      if (!document.hidden && currentTab === 'accounts') refreshAccounts();
    }, 15000);
    setInterval(function () {
      if (!document.hidden && currentTab === 'usage') refreshUsage();
    }, 10000);
    setInterval(function () {
      if (!document.hidden && currentTab === 'scheduler') refreshScheduler();
    }, 30000);
    // 顶栏状态低频刷新，任何页面都更新
    setInterval(function () {
      if (document.hidden) return;
      requestJSON('/panel/api/overview?days=1', null, 12000).then(function (r) {
        if (r.ok && r.data) renderChips(r.data.service || {}, r.data.accounts || {});
      });
    }, 20000);
  }

  document.addEventListener('visibilitychange', function () {
    if (!document.hidden && currentTab === 'overview') refreshOverview();
  });

  // ---------- 打赏弹窗 ----------

  // 移动端侧栏底部折叠成顶栏，底部入口不可见，需要顶栏补一个。
  // 用 matchMedia 而不是 CSS 隐藏：省掉一个隐藏按钮的冗余判断。
  var mqMobile = window.matchMedia('(max-width: 820px)');
  function syncRewardEntry() {
    $('btn-reward-top').hidden = !mqMobile.matches;
  }
  mqMobile.addEventListener('change', syncRewardEntry);
  syncRewardEntry();

  var lastFocused = null;

  function openReward() {
    lastFocused = document.activeElement;
    $('reward-modal').hidden = false;
    // 聚焦到关闭按钮：键盘用户一进来就能 Esc 或回车关闭
    $('btn-reward-close').focus();
  }

  function closeReward() {
    $('reward-modal').hidden = true;
    if (lastFocused && lastFocused.focus) lastFocused.focus();
  }

  $('btn-reward').addEventListener('click', openReward);
  $('btn-reward-top').addEventListener('click', openReward);
  $('btn-reward-close').addEventListener('click', closeReward);

  // 点遮罩空白处关闭（点弹窗本体不关，避免误触）
  $('reward-modal').addEventListener('click', function (e) {
    if (e.target === this) closeReward();
  });

  // Esc 关闭
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && !$('reward-modal').hidden) closeReward();
  });

  // 微信 / 支付宝切换
  document.querySelectorAll('.reward-tab').forEach(function (btn) {
    btn.addEventListener('click', function () {
      document.querySelectorAll('.reward-tab').forEach(function (b) {
        b.classList.toggle('active', b === btn);
      });
      $('reward-img').src = btn.dataset.src;
      $('reward-label').textContent = btn.dataset.label;
    });
  });

  // ---------- 启动 ----------

  applyTheme(document.documentElement.getAttribute('data-theme') || 'light', false);
  var initial = (location.hash || '').replace('#', '');
  switchTab(initial || 'overview');
  loadSettings();
  startPolling();
})();
