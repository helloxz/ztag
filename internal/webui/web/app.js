// ZTAG 管理台公共脚本：统一请求封装、提示、小工具。
// 各页面在自身的内联脚本里调用这些函数。

// apiCall 调用管理接口并解包统一响应 {"code":200,"msg":"","data":...}。
// 401 表示会话失效，直接跳登录页；其余失败抛出带 msg 的 Error。
async function apiCall(path, options) {
  const init = Object.assign({ credentials: 'same-origin', headers: {} }, options || {});
  if (init.body !== undefined && typeof init.body !== 'string') {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(init.body);
  }

  const resp = await fetch(path, init);
  if (resp.status === 401) {
    window.location.href = '/admin/login';
    throw new Error('请重新登录');
  }

  let payload = null;
  try {
    payload = await resp.json();
  } catch (_) {
    // 非 JSON 响应（如 404 页面）按失败处理
  }
  if (!payload || payload.code !== 200) {
    const msg = (payload && payload.msg) || '请求失败（HTTP ' + resp.status + '）';
    throw new Error(msg);
  }
  return payload.data;
}

// showMsg 显示 / 隐藏提示条（复用 oat 的 [role=alert]）。
// 成功提示 5 秒后自动消失，错误/警告保留到下一次操作。
let showMsgTimer = null;

function showMsg(el, text, variant) {
  if (!el) return;
  if (showMsgTimer) {
    clearTimeout(showMsgTimer);
    showMsgTimer = null;
  }
  if (!text) {
    el.hidden = true;
    el.textContent = '';
    return;
  }
  el.textContent = text;
  el.dataset.variant = variant || 'error';
  el.hidden = false;
  if (variant === 'success') {
    showMsgTimer = setTimeout(() => {
      el.hidden = true;
      el.textContent = '';
    }, 5000);
  }
}

// $ 简化 document.getElementById。
function $(id) {
  return document.getElementById(id);
}

// splitList 把逗号 / 换行分隔的文本切成数组（去空、去重、保序）。
function splitList(text) {
  const seen = new Set();
  return String(text || '')
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter((s) => s && !seen.has(s) && seen.add(s));
}

// joinLines 把数组拼成换行文本，便于放进 textarea。
function joinLines(list) {
  return (list || []).join('\n');
}
