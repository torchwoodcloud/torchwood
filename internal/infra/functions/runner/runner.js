/*
 * Torchwood functions runner（执行器 v4：实例内多路复用 + Web 标准 fetch
 * 接口，docs/design/functions-v3.md §1.2/§2.1–§2.4；前身 v3 常驻并发模型、
 * v2 常驻模型 P0.5）。
 *
 * 容器 CMD 即本文件（镜像模板无 ENTRYPOINT，用户代码不进 CMD）——
 * 「构建期不执行用户代码」的不变量保持：本文件在构建期仅被 COPY。
 *
 *   - 启动即同步 require 用户模块（约定 ./index.js），入口探测（v4 §2.1，
 *     优先级固定）：mod.fetch 为 function → fetch 风格；否则 mod.main 为
 *     function → main 风格（既有路径，行为不变）；两者皆无 → 加载错误
 *     「index.js must export main or fetch」（加载失败常驻 not-ready，
 *     dispatcher 的 boot 探针超时后回收实例并向上报错）。CJS 约定：
 *     `module.exports = { fetch }` 或 `exports.fetch`——设计示例是 ESM
 *     语义（export default { fetch }），runner 用 require 只消费 CJS，
 *     ESM 写法需经打包/互操作落成 CJS 导出；
 *   - POST /：两种风格（D9 双轨，同一函数包二选一生效）：
 *       · main 风格：body = TW_DATA JSON，main(data, ctx)。ctx =
 *         { executionToken, apiBaseUrl, executionId }——并发下唯一安全的
 *         请求上下文通道（AsyncLocalStorage 圈住每次调用，v3 §1.2 凭证
 *         串号修复）。process.env.TW_EXECUTION_TOKEN 仍逐请求同步写入
 *         （同步 main 与模块顶层读取兼容）；但含 await 的函数恢复执行后
 *         env 可能已被后续请求覆盖——**必须读 ctx**；
 *       · fetch 风格（v4 §2.1）：fetch(request, env)，env =
 *         { EXECUTION_TOKEN, API_BASE_URL, EXECUTION_ID }——请求级三件
 *         经参数传递（v3 §2.1：token 串号问题在该风格下结构性不存在；
 *         functionVariables 仍固化在容器 process.env，函数级非请求级）。
 *         request 是 Node 18+ 原生全局 Request（undici）；
 *   - 触发器封套（v4 §2.3，对抗审查修正：封套经独立 header 传递）：分发
 *     header `x-tw-trigger-envelope`（base64 JSON：{method,path,raw_query,
 *     headers 白名单}，不含 body，≤12KB）存在时——fetch 风格还原 Request：
 *     url = http://trigger{path}?{raw_query}、headers 原样、body = 分发
 *     HTTP body 本身（封套模式下 dispatcher 发的是触发器原始 body 而非
 *     TW_DATA）；main 风格封套注入：用元数据 + body 重组 TW_DATA（与
 *     handler 构建的封套逐字段同构，行为与 v3 现状等价）。无该 header：
 *     fetch 风格 Request = POST http://function/ body=TW_DATA（§2.2
 *     server/client invoke 与 cron 语义）；main 风格照旧 TW_DATA；
 *   - Response 处理（v4 §2.2/§2.4）：`await response.arrayBuffer()` 全缓冲
 *     （流式不做，OQ7 收口）；fetch 风格成功封套扩展为
 *     { ok, status, headers, body_base64, truncated }——status/headers/
 *     body_base64 恒在（无损二进制）；headers 仅收函数设置的（排除
 *     hop-by-hop：connection/content-length/transfer-encoding/keep-alive/
 *     host/date/server——runner 自身响应头不在 Response 内）；body 超 64KB
 *     截断标 truncated（沿平台 maxOutputBytes 输出口径）。main 风格封套
 *     保持 {ok, result, stdout, stderr} 不变（部署与模板同版本化，无向后
 *     兼容窗口）；
 *   - console.* 捕获按请求分桶（v3 §1.2）：优先写本请求环缓冲（各自 64KB
 *     尾部），模块加载期（无请求上下文）落实例级兜底；响应的 stdout/stderr
 *     字段为「本请求」console 输出尾部；
 *   - per-request 超时（v3 §1.2）：分发 header x-tw-timeout-seconds 携带
 *     函数超时（缺省 30s）；到点 promise 未决议 → 回 500 封套（error
 *     注明 timed out）并放弃等待，对两种风格一致生效。诚实声明（与 Lambda
 *     同款）：超时后用户代码可能仍在事件循环里跑至实例回收——inflight 按
 *     「请求生命周期」释放，不追踪用户代码生命周期。响应写回时调用方可能
 *     已断开（dispatcher ctx 超时先行），ECONNRESET/写后错误一律吞掉；
 *   - 自回收与信号不变（v2 既有）：每响应后计数达 TW_MAX_REQUESTS 主动
 *     退出（dispatcher 检测退出后补位）；SIGTERM → 停止接新请求、等在途
 *     完成后退出（drain），TW_DRAIN_TIMEOUT 兜底强退。
 */
'use strict';

const http = require('http');
const path = require('path');
const { AsyncLocalStorage } = require('node:async_hooks');

const PORT = Number(process.env.TW_RUNNER_PORT || 18080);
const MAX_REQUESTS = Number(process.env.TW_MAX_REQUESTS || 1000);
const DRAIN_TIMEOUT_MS = Number(process.env.TW_DRAIN_TIMEOUT_MS || 10000);
// per-request 超时缺省值（分发 header x-tw-timeout-seconds 缺省时；v3 §1.2）。
const DEFAULT_REQUEST_TIMEOUT_S = 30;
// 请求体上限：普通调用 data ≤ 32KB；HTTP 触发器封套模式下 body 为触发器
// 原始 body（≤1MB，平台 body_limit 口径）+ main 风格封套注入后的 TW_DATA
//（双编码 ≈ 2.4MB）。取 4MB 覆盖（与 app 层 maxTriggerDataBytes 同源）。
const MAX_BODY_BYTES = 4 * 1024 * 1024;
// console.* 环缓冲尾部与响应 body 截断上限（与平台 64KB 输出截断口径对齐；
// per-request 分桶与实例级兜底各自独立 64KB）。
const LOG_TAIL_BYTES = 64 * 1024;
const MAX_OUTPUT_BYTES = 64 * 1024;

// v4 §2.2：fetch 风格响应头过滤清单（hop-by-hop + 平台头）。函数 Response
// 设置的这些头不进封套——content-length 由分发链路按实际 body 重算，date/
// server/host 不得由用户代码冒充。其余头（含 content-type）原样透传。
const RESPONSE_HEADER_BLOCKLIST = new Set([
  'connection',
  'keep-alive',
  'proxy-connection',
  'te',
  'trailer',
  'transfer-encoding',
  'upgrade',
  'content-length',
  'host',
  'date',
  'server',
]);

// als 圈住每次调用的请求上下文（v3 §1.2）：store = { token, apiBaseUrl,
// executionId, logs }；console 捕获经 als.getStore() 定位本请求分桶。
const als = new AsyncLocalStorage();

let userMain = null;
let userFetch = null;
let loadError = null;
let shuttingDown = false;
let inFlight = 0;
let served = 0;

// ---- console.* 捕获（日志语义：照常写容器 stdout，同时保留尾部环缓冲）----

// 实例级兜底缓冲：仅模块加载期（无请求上下文）使用；请求期一律写本请求
// 分桶（store.logs）。
const logTail = { out: Buffer.alloc(0), err: Buffer.alloc(0) };

function appendTail(buf, chunk) {
  const merged = Buffer.concat([buf, Buffer.isBuffer(chunk) ? chunk : Buffer.from(String(chunk))]);
  return merged.length > LOG_TAIL_BYTES ? merged.subarray(merged.length - LOG_TAIL_BYTES) : merged;
}

function patchConsole(method, key) {
  const orig = console[method].bind(console);
  console[method] = (...args) => {
    orig(...args);
    try {
      const line = args.map((a) => {
        if (typeof a === 'string') return a;
        try { return JSON.stringify(a); } catch (_) { return String(a); }
      }).join(' ') + '\n';
      const store = als.getStore();
      const bucket = store ? store.logs : logTail;
      bucket[key] = appendTail(bucket[key], line);
    } catch (_) { /* 日志捕获永不影响执行 */ }
  };
}

patchConsole('log', 'out');
patchConsole('info', 'out');
patchConsole('warn', 'err');
patchConsole('error', 'err');

// ---- 用户模块加载（启动即加载；失败常驻 not-ready，由 dispatcher 回收） ----

try {
  // 沿用 v1 约定：工作目录下 index.js。
  // eslint-disable-next-line security/detect-non-literal-require
  const mod = require(path.join(process.cwd(), 'index.js'));
  // v4 §2.1 入口探测（优先级固定）：fetch → main，二者皆无 = 加载错误。
  if (typeof mod.fetch === 'function') {
    userFetch = mod.fetch;
  } else if (typeof mod.main === 'function') {
    userMain = mod.main;
  } else {
    loadError = 'index.js must export main or fetch';
  }
} catch (e) {
  loadError = 'load user module failed: ' + (e && e.stack ? e.stack : String(e));
}

// logsFor 返回响应封套的 stdout/stderr 字段：有请求上下文（或显式传 store）
// 时为「本请求」分桶尾部，否则回落实例级兜底缓冲（v3 §1.2 分桶语义）。
function logsFor(store) {
  const src = store ? store.logs : logTail;
  return {
    stdout: src.out.toString('utf8'),
    stderr: src.err.toString('utf8'),
  };
}

function sendJSON(res, status, payload) {
  try {
    if (res.headersSent) {
      try { res.end(); } catch (_) { /* ignore */ }
      return;
    }
    const body = Buffer.from(JSON.stringify(payload));
    res.writeHead(status, {
      'Content-Type': 'application/json',
      'Content-Length': body.length,
    });
    res.end(body);
  } catch (_) {
    // v3 §1.2：响应写回时调用方可能已断开（dispatcher ctx 超时先行 →
    // ECONNRESET/写后错误）——吞掉，不影响实例与 inflight 收账。
  }
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

// buildTWData 组装 main 风格 TW_DATA：无封套 = 分发 body 即 TW_DATA JSON
// （v3 现状，server/client invoke 与 cron 语义不变）；有封套 = 封套注入
// （v4 §2.3「main 风格，与现状等价」/D9 双轨）：元数据来自分发 header，
// body/body_base64 由分发 HTTP body 重建——与 handler 构建的封套逐字段
// 同构（body best-effort UTF-8：非法字节 U+FFFD；body_base64 无损）。
function buildTWData(envelope, raw) {
  if (!envelope) {
    const text = raw.toString('utf8');
    return text.trim() === '' ? {} : JSON.parse(text);
  }
  return {
    method: envelope.method,
    path: envelope.path,
    raw_query: envelope.raw_query || '',
    headers: envelope.headers || {},
    body: raw.toString('utf8'),
    body_base64: raw.toString('base64'),
  };
}

// buildRequest 构造 fetch 风格 Request（v4 §2.3 封套还原 / §2.2 invoke 语义）：
// 有封套 → url = http://trigger{path}?{raw_query}、headers 白名单原样
// （多值展平为 pairs 交给 Headers）、body = 分发 HTTP body 本身（封套模式
// 下 dispatcher 发的是触发器原始 body；GET/HEAD 不允许 body，置空）；
// 无封套 → POST http://function/ body=TW_DATA。
function buildRequest(envelope, raw) {
  if (!envelope) {
    return new Request('http://function/', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: raw,
    });
  }
  const url = new URL('http://trigger' + (envelope.path || '/') + (envelope.raw_query ? '?' + envelope.raw_query : ''));
  const method = String(envelope.method || 'GET').toUpperCase();
  const headers = [];
  const hs = envelope.headers || {};
  for (const key of Object.keys(hs)) {
    const vals = Array.isArray(hs[key]) ? hs[key] : [hs[key]];
    for (const v of vals) headers.push([key, String(v)]);
  }
  const init = { method, headers };
  if (method !== 'GET' && method !== 'HEAD' && raw.length > 0) {
    init.body = raw;
  }
  return new Request(url, init);
}

// finishFetchResponse 序列化 fetch 风格 Response（v4 §2.2/§2.4）：
// arrayBuffer 全缓冲（流式不做，OQ7 收口）；body 超 64KB 截断标 truncated
// （对二进制同样生效）；headers 仅函数设置的（过滤清单见
// RESPONSE_HEADER_BLOCKLIST）；封套 {ok, status, headers, body_base64,
// truncated}——status/headers/body_base64 恒在，无损承载二进制响应。
function finishFetchResponse(store, response, finish) {
  if (!(response instanceof Response)) {
    finish(500, Object.assign(logsFor(store), { ok: false, error: 'fetch handler must return a Response' }));
    return;
  }
  response.arrayBuffer().then(
    (buf) => {
      let body = Buffer.from(buf);
      let truncated = false;
      if (body.length > MAX_OUTPUT_BYTES) {
        body = body.subarray(0, MAX_OUTPUT_BYTES);
        truncated = true;
      }
      const headers = {};
      try {
        // Headers 迭代的名字已按规范小写；同名多值合并为逗号连接（规范
        // combined 值；Set-Cookie 类单值语义例外一期不做，见 §2.4 一期边界）。
        response.headers.forEach((value, name) => {
          const ln = String(name).toLowerCase();
          if (RESPONSE_HEADER_BLOCKLIST.has(ln)) return;
          headers[ln] = value;
        });
      } catch (_) { /* headers 枚举失败不致命 */ }
      finish(200, Object.assign(logsFor(store), {
        ok: true,
        status: response.status,
        headers,
        body_base64: body.toString('base64'),
        truncated,
      }));
    },
    (e) => {
      finish(500, Object.assign(logsFor(store), {
        ok: false,
        error: 'read Response body failed: ' + String(e && e.message ? e.message : e),
      }));
    },
  );
}

// finishMainResult 序列化 main 风格返回值（v3 现状不变）：封套
// {ok, result, stdout, stderr}。
function finishMainResult(store, r, finish) {
  let payload;
  try {
    // undefined/循环引用等不可序列化返回值归一为 null/错误。
    payload = JSON.parse(JSON.stringify(r === undefined ? null : r));
  } catch (e) {
    finish(500, Object.assign(logsFor(store), { ok: false, error: 'main() result is not JSON-serializable: ' + String(e && e.message) }));
    return;
  }
  finish(200, Object.assign(logsFor(store), { ok: true, result: payload }));
}

const server = http.createServer((req, res) => {
  // 连接可能中途断开（dispatcher ctx 超时先行）：响应流错误不致命，静默吞掉。
  res.on('error', () => {});
  if (req.method === 'GET' && req.url === '/_tw/health') {
    if (userFetch || userMain) {
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
  if (!userFetch && !userMain) {
    sendJSON(res, 500, Object.assign(logsFor(null), { ok: false, error: loadError || 'user module not loaded' }));
    return;
  }
  if (shuttingDown) {
    // drain 中不再接新请求（连接被拒后由调用方重试到其他实例）。
    sendJSON(res, 503, Object.assign(logsFor(null), { ok: false, error: 'instance draining' }));
    return;
  }

  const chunks = [];
  let size = 0;
  let aborted = false;
  req.on('data', (c) => {
    size += c.length;
    if (size > MAX_BODY_BYTES) {
      aborted = true;
      sendJSON(res, 413, Object.assign(logsFor(null), { ok: false, error: 'request body too large' }));
      return;
    }
    chunks.push(c);
  });
  req.on('end', () => {
    if (aborted) return;
    inFlight++;
    const raw = Buffer.concat(chunks);

    // v4 §2.3：触发器封套元数据（分发 header，base64 JSON，不含 body）。
    // 仅 dispatcher 设置；解析失败视为不存在（内网可信调用方，异常形态
    // 交由调用方协议约束）。
    let triggerEnvelope = null;
    const envelopeHeader = req.headers['x-tw-trigger-envelope'];
    if (typeof envelopeHeader === 'string' && envelopeHeader.length > 0) {
      try {
        triggerEnvelope = JSON.parse(Buffer.from(envelopeHeader, 'base64').toString('utf8'));
      } catch (_) {
        triggerEnvelope = null;
      }
    }

    // 双轨分流（v4 §2.1/D9）：fetch 风格还原 Request；main 风格组装
    // TW_DATA（有封套 = 封套注入，与 v3 现状等价）。
    let data;
    let request = null;
    try {
      if (userFetch) {
        request = buildRequest(triggerEnvelope, raw);
      } else {
        data = buildTWData(triggerEnvelope, raw);
      }
    } catch (e) {
      inFlight--;
      served++;
      sendJSON(res, 400, Object.assign(logsFor(null), {
        ok: false,
        error: 'invalid request: ' + String(e && e.message ? e.message : e),
      }));
      maybeRecycle();
      return;
    }

    // 执行身份注入（P0.5 通道 + v3 兼容）：token 经分发 header 传入，仍逐
    // 请求「同步」写入 process.env（同步 main 兼容——同步执行期间事件循环
    // 不交错故安全）；含 await 的函数恢复后 env 可能已被后续请求覆盖，
    // 必须读 ctx.executionToken / env.EXECUTION_TOKEN（v3 §1.2 凭证串号
    // 修复；v4 §2.1 fetch 风格 env 参数结构性无串号）。
    let token = req.headers['x-tw-execution-token'];
    if (typeof token !== 'string') token = '';
    if (token.length > 0) {
      process.env.TW_EXECUTION_TOKEN = token;
    } else {
      delete process.env.TW_EXECUTION_TOKEN;
    }

    // v3 §1.2：per-request 超时（分发 header 缺省 30s）+ 请求上下文分桶。
    const timeoutHeader = Number(req.headers['x-tw-timeout-seconds']);
    const timeoutS = Number.isFinite(timeoutHeader) && timeoutHeader > 0 ? timeoutHeader : DEFAULT_REQUEST_TIMEOUT_S;
    let executionId = req.headers['x-tw-execution-id'];
    if (typeof executionId !== 'string') executionId = '';
    const store = {
      token,
      apiBaseUrl: process.env.TW_API_BASE_URL || '',
      executionId,
      logs: { out: Buffer.alloc(0), err: Buffer.alloc(0) },
    };
    // v3 §1.2 调用约定：main(data, ctx)。ctx 是并发安全的请求上下文通道。
    const ctx = { executionToken: store.token, apiBaseUrl: store.apiBaseUrl, executionId: store.executionId };
    // v4 §2.1 调用约定：fetch(request, env)。env 是每次调用的参数而非
    // process.env——请求级三件与 ctx 同源（token/apiBaseUrl/executionId）。
    const env = { EXECUTION_TOKEN: store.token, API_BASE_URL: store.apiBaseUrl, EXECUTION_ID: store.executionId };
    const entryName = userFetch ? 'fetch()' : 'main()';

    let settled = false;
    const finish = (status, payload) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      inFlight--;
      served++;
      sendJSON(res, status, payload);
      maybeRecycle();
    };
    // per-request 超时（v3 §1.2）：到点 promise 未决议 → 回 500 封套并放弃
    // 等待，两种风格一致生效。诚实声明：用户代码可能继续跑到实例回收
    // （僵尸 async 操作的兜底在 dispatcher 侧超时熔断，v3 §1.4）；inflight
    // 按请求生命周期在此释放，不追踪用户代码生命周期。
    const timer = setTimeout(() => {
      finish(500, Object.assign(logsFor(store), {
        ok: false,
        error: entryName + ' timed out after ' + timeoutS + 's (runner abandoned the request; the function may keep running until the instance is recycled)',
      }));
    }, timeoutS * 1000);

    als.run(store, () => {
      let pending;
      try {
        pending = userFetch ? userFetch(request, env) : userMain(data, ctx);
      } catch (e) {
        finish(500, Object.assign(logsFor(store), { ok: false, error: String(e && e.stack ? e.stack : e) }));
        return;
      }
      // .then 注册在 als.run 内：回调确定运行在本请求上下文（console 分桶
      // 依赖此），finish 显式携带 store 不依赖 ALS。
      Promise.resolve(pending).then(
        (r) => {
          if (userFetch) {
            finishFetchResponse(store, r, finish);
          } else {
            finishMainResult(store, r, finish);
          }
        },
        (e) => {
          finish(500, Object.assign(logsFor(store), { ok: false, error: String(e && e.stack ? e.stack : e) }));
        },
      );
    });
  });
  req.on('error', () => { aborted = true; });
});

server.listen(PORT, '0.0.0.0', () => {
  process.stdout.write(JSON.stringify({ tw_runner: 'listening', port: PORT, ready: !!(userFetch || userMain), style: userFetch ? 'fetch' : (userMain ? 'main' : null) }) + '\n');
});
