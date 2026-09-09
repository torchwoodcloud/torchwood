/*
 * Torchwood functions runner（执行器 v2 常驻执行模型，P0.5）。
 *
 * 设计：docs/design/functions-execution-identity-and-triggers.md §6。
 * 容器 CMD 即本文件（镜像模板无 ENTRYPOINT，用户代码不进 CMD）：
 *   - 启动即同步 require 用户模块（约定 ./index.js 导出 main(TW_DATA)）；
 *     加载完成前 /_tw/health 返回 not-ready（加载失败常驻 not-ready，
 *     dispatcher 的 boot 探针超时后回收实例并向上报错）；
 *   - POST / ：body = TW_DATA JSON；header x-tw-execution-token 携带本次
 *     执行的平台短期凭证——每请求把 header 值写入 process.env.TW_EXECUTION_TOKEN
 *     再调 main。一期串行执行（1 并发/实例，dispatcher 保证同实例同时只有
 *     一个在途请求），因此逐请求覆盖 env 是安全的；放开多路复用（Q12）前
 *     必须改为按请求上下文传递；
 *   - 响应协议：200 {"ok":true,"result":<main 返回值>} /
 *     500 {"ok":false,"error":...}；附加 stdout/stderr 字段为 console.* 的
 *     尾部环缓冲（日志语义，dispatcher 原样透传为 stdout_tail/stderr_tail）；
 *   - 自回收：每响应后计数，达 TW_MAX_REQUESTS 主动退出（dispatcher 检测
 *     退出后补位）；SIGTERM → 停止接新请求、等在途完成后退出（drain），
 *     TW_DRAIN_TIMEOUT 兜底强退。
 * 构建期不执行用户代码的不变量保持：本文件在构建期仅被 COPY。
 */
'use strict';

const http = require('http');
const path = require('path');

const PORT = Number(process.env.TW_RUNNER_PORT || 18080);
const MAX_REQUESTS = Number(process.env.TW_MAX_REQUESTS || 1000);
const DRAIN_TIMEOUT_MS = Number(process.env.TW_DRAIN_TIMEOUT_MS || 10000);
// 请求体上限：平台契约 data ≤ 32KB，这里放宽到 128KB 防御异常调用方。
const MAX_BODY_BYTES = 128 * 1024;
// console.* 环缓冲尾部上限（与平台 stdout 64KB 截断口径对齐）。
const LOG_TAIL_BYTES = 64 * 1024;

let userMain = null;
let loadError = null;
let shuttingDown = false;
let inFlight = 0;
let served = 0;

// ---- console.* 捕获（日志语义：照常写容器 stdout，同时保留尾部环缓冲） ----

const logTail = { out: Buffer.alloc(0), err: Buffer.alloc(0) };

function appendTail(buf, chunk) {
  const merged = Buffer.concat([buf, Buffer.isBuffer(chunk) ? chunk : Buffer.from(String(chunk))]);
  return merged.length > LOG_TAIL_BYTES ? merged.subarray(merged.length - LOG_TAIL_BYTES) : merged;
}

function patchConsole(method, sink, key) {
  const orig = console[method].bind(console);
  console[method] = (...args) => {
    orig(...args);
    try {
      const line = args.map((a) => {
        if (typeof a === 'string') return a;
        try { return JSON.stringify(a); } catch (_) { return String(a); }
      }).join(' ') + '\n';
      logTail[key] = appendTail(logTail[key], line);
    } catch (_) { /* 日志捕获永不影响执行 */ }
    void sink;
  };
}

patchConsole('log', process.stdout, 'out');
patchConsole('info', process.stdout, 'out');
patchConsole('warn', process.stderr, 'err');
patchConsole('error', process.stderr, 'err');

// ---- 用户模块加载（启动即加载；失败常驻 not-ready，由 dispatcher 回收） ----

try {
  // 沿用 v1 约定：工作目录下 index.js 导出 main(TW_DATA)。
  // eslint-disable-next-line security/detect-non-literal-require
  const mod = require(path.join(process.cwd(), 'index.js'));
  if (typeof mod.main !== 'function') {
    loadError = 'index.js does not export a main function';
  } else {
    userMain = mod.main;
  }
} catch (e) {
  loadError = 'load user module failed: ' + (e && e.stack ? e.stack : String(e));
}

function tailJSON(extra) {
  return Object.assign({
    stdout: logTail.out.toString('utf8'),
    stderr: logTail.err.toString('utf8'),
  }, extra);
}

function sendJSON(res, status, payload) {
  if (res.headersSent) {
    res.end();
    return;
  }
  const body = Buffer.from(JSON.stringify(payload));
  res.writeHead(status, {
    'Content-Type': 'application/json',
    'Content-Length': body.length,
  });
  res.end(body);
}

function maybeRecycle() {
  if (MAX_REQUESTS > 0 && served >= MAX_REQUESTS && !shuttingDown) {
    // max_requests 到期：本响应已回传，进程退出由 dispatcher 检测并补位。
    process.exit(0);
  }
}

function gracefulExit() {
  shuttingDown = true;
  server.close(() => process.exit(0));
  // 在途请求排空兜底：drain 上限到点强退（dispatcher 侧同样有 force kill）。
  setTimeout(() => process.exit(0), DRAIN_TIMEOUT_MS).unref();
}

process.on('SIGTERM', gracefulExit);
process.on('SIGINT', gracefulExit);

const server = http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/_tw/health') {
    if (userMain) {
      sendJSON(res, 200, { ok: true, ready: true, served, inflight: inFlight });
    } else {
      sendJSON(res, 503, { ok: false, ready: false, error: loadError || 'loading' });
    }
    return;
  }
  if (req.method !== 'POST' || req.url !== '/') {
    sendJSON(res, 404, { ok: false, error: 'not found' });
    return;
  }
  if (!userMain) {
    sendJSON(res, 500, tailJSON({ ok: false, error: loadError || 'user module not loaded' }));
    return;
  }
  if (shuttingDown) {
    // drain 中不再接新请求（连接被拒后由调用方重试到其他实例）。
    sendJSON(res, 503, tailJSON({ ok: false, error: 'instance draining' }));
    return;
  }

  const chunks = [];
  let size = 0;
  let aborted = false;
  req.on('data', (c) => {
    size += c.length;
    if (size > MAX_BODY_BYTES) {
      aborted = true;
      sendJSON(res, 413, tailJSON({ ok: false, error: 'request body too large' }));
      return;
    }
    chunks.push(c);
  });
  req.on('end', () => {
    if (aborted) return;
    inFlight++;
    let data;
    try {
      const raw = Buffer.concat(chunks).toString('utf8');
      data = raw.trim() === '' ? {} : JSON.parse(raw);
    } catch (e) {
      inFlight--;
      served++;
      sendJSON(res, 400, tailJSON({ ok: false, error: 'invalid TW_DATA JSON: ' + String(e && e.message) }));
      maybeRecycle();
      return;
    }
    // 执行身份注入（P0.5 通道切换）：token 经分发 header 传入，逐请求写入
    // process.env（一期串行执行使覆盖安全，见文件头注释）。
    const token = req.headers['x-tw-execution-token'];
    if (typeof token === 'string' && token.length > 0) {
      process.env.TW_EXECUTION_TOKEN = token;
    } else {
      delete process.env.TW_EXECUTION_TOKEN;
    }

    let result;
    try {
      result = userMain(data);
    } catch (e) {
      inFlight--;
      served++;
      sendJSON(res, 500, tailJSON({ ok: false, error: String(e && e.stack ? e.stack : e) }));
      maybeRecycle();
      return;
    }
    Promise.resolve(result).then(
      (r) => {
        let payload;
        try {
          // undefined/循环引用等不可序列化返回值归一为 null/错误。
          payload = JSON.parse(JSON.stringify(r === undefined ? null : r));
        } catch (e) {
          inFlight--;
          served++;
          sendJSON(res, 500, tailJSON({ ok: false, error: 'main() result is not JSON-serializable: ' + String(e && e.message) }));
          maybeRecycle();
          return;
        }
        inFlight--;
        served++;
        sendJSON(res, 200, tailJSON({ ok: true, result: payload }));
        maybeRecycle();
      },
      (e) => {
        inFlight--;
        served++;
        sendJSON(res, 500, tailJSON({ ok: false, error: String(e && e.stack ? e.stack : e) }));
        maybeRecycle();
      },
    );
  });
  req.on('error', () => { aborted = true; });
});

server.listen(PORT, '0.0.0.0', () => {
  process.stdout.write(JSON.stringify({ tw_runner: 'listening', port: PORT, ready: !!userMain }) + '\n');
});
