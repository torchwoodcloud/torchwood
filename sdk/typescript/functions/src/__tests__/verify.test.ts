import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  CANONICAL_SIGNATURE_VECTOR,
  PROTOCOL_VERSION,
  SIGNATURE_TIMESTAMP_TOLERANCE_MS,
  buildSignatureHeader,
  computeSignature,
  parseSignatureHeader,
  signaturePayload,
  verify,
  verifySignatureHeader,
} from "../verify.js";

const utf8 = (s: string): Uint8Array => new TextEncoder().encode(s);

describe("签名方案（对齐 Go verify.go / 平台 protocol.go）", () => {
  it("跨仓基准向量：HMAC-SHA256(whsec_vector, \"1759000000.abc\")，任一侧口径漂移即红", () => {
    assert.equal(computeSignature("whsec_vector", 1759000000, utf8("abc")), CANONICAL_SIGNATURE_VECTOR);
  });

  it("载荷串口径：<unix秒>.<body>（单一函数，禁止手拼）", () => {
    assert.deepEqual(Array.from(signaturePayload(1759000000, utf8("abc"))), Array.from(utf8("1759000000.abc")));
    // 二进制 body 逐字节保真（Go string(body) 同款）。
    const bytes = new Uint8Array([0, 1, 0xfe, 0xff]);
    const payload = signaturePayload(1, bytes);
    assert.deepEqual(Array.from(payload.subarray(2)), Array.from(bytes));
  });

  it("buildSignatureHeader：t= 前缀 + v1/v2 条目逗号连接", () => {
    assert.equal(buildSignatureHeader(1759000000, [["v1", "aa"], ["v2", "bb"]]), "t=1759000000,v1=aa,v2=bb");
    assert.equal(buildSignatureHeader(1, []), "t=1");
  });

  it("parseSignatureHeader：解析回环 + 无关键 / 空片段跳过（Go strings.Cut 同款）", () => {
    const { timestamp, signatures } = parseSignatureHeader(" t=1759000000 , v1=aa , junk , =x , v2=bb ,");
    assert.equal(timestamp, 1759000000);
    assert.deepEqual(Object.fromEntries(signatures), { v1: "aa", v2: "bb" });
  });

  it("parseSignatureHeader：同名条目后者覆盖（Go map 语义）", () => {
    const { signatures } = parseSignatureHeader("t=1,v1=first,v1=second");
    assert.equal(signatures.get("v1"), "second");
  });

  it("parseSignatureHeader：缺 t / 非法时间戳 / 空签名 fail-closed", () => {
    assert.throws(() => parseSignatureHeader("v1=aa"), /missing t/);
    assert.throws(() => parseSignatureHeader(""), /missing t/);
    assert.throws(() => parseSignatureHeader("t=abc,v1=aa"), /invalid timestamp/);
    assert.throws(() => parseSignatureHeader("t=1x,v1=aa"), /invalid timestamp/);
    assert.throws(() => parseSignatureHeader("t=1759000000"), /no signature entries/);
    assert.throws(() => parseSignatureHeader("t=1759000000,v1="), /no signature entries/);
  });

  it("verifySignatureHeader：正确密钥 + 时间窗内 → 通过（多密钥任一命中）", () => {
    const ts = 1759000000;
    const body = utf8("abc");
    const header = buildSignatureHeader(ts, [["v1", computeSignature("s1", ts, body)]]);
    assert.doesNotThrow(() => verifySignatureHeader(header, body, ["s0", "s1"], new Date(ts * 1000)));
  });

  it("verifySignatureHeader：轮换窗口——头带 v1 新钥 / v2 旧钥，持旧钥集可验", () => {
    const ts = 1759000000;
    const body = utf8("abc");
    const header = buildSignatureHeader(ts, [
      ["v1", computeSignature("whsec_new", ts, body)],
      ["v2", computeSignature("whsec_old", ts, body)],
    ]);
    assert.doesNotThrow(() => verifySignatureHeader(header, body, ["whsec_old"], new Date(ts * 1000)));
  });

  it("verifySignatureHeader：±5 分钟时间窗（边界内过 / 窗外拒）", () => {
    const ts = 1759000000;
    const body = utf8("abc");
    const header = buildSignatureHeader(ts, [["v1", computeSignature("s", ts, body)]]);
    const toleranceSec = SIGNATURE_TIMESTAMP_TOLERANCE_MS / 1000;
    assert.doesNotThrow(() => verifySignatureHeader(header, body, ["s"], new Date((ts + toleranceSec) * 1000)));
    assert.doesNotThrow(() => verifySignatureHeader(header, body, ["s"], new Date((ts - toleranceSec) * 1000)));
    assert.throws(
      () => verifySignatureHeader(header, body, ["s"], new Date((ts + toleranceSec + 1) * 1000)),
      /outside tolerance/
    );
    assert.throws(
      () => verifySignatureHeader(header, body, ["s"], new Date((ts - toleranceSec - 1) * 1000)),
      /outside tolerance/
    );
  });

  it("verifySignatureHeader：错误密钥 / 篡改 body / 篡改签名 → no signature match", () => {
    const ts = 1759000000;
    const body = utf8("abc");
    const header = buildSignatureHeader(ts, [["v1", computeSignature("s", ts, body)]]);
    assert.throws(
      () => verifySignatureHeader(header, utf8("abd"), ["s"], new Date(ts * 1000)),
      /no signature match/
    );
    assert.throws(
      () => verifySignatureHeader(header, body, ["other"], new Date(ts * 1000)),
      /no signature match/
    );
    const good = computeSignature("s", ts, body);
    const tampered = buildSignatureHeader(ts, [
      ["v1", (good[0] === "0" ? "1" : "0") + good.slice(1)],
    ]);
    assert.throws(
      () => verifySignatureHeader(tampered, body, ["s"], new Date(ts * 1000)),
      /no signature match/
    );
  });

  it("verify 中间件：空密钥集构造即抛（fail-closed——无密钥等于不设防）", () => {
    assert.throws(() => verify(), /requires at least one secret/);
  });

  it("PROTOCOL_VERSION = 1（与平台 domain protocol.go 同值）", () => {
    assert.equal(PROTOCOL_VERSION, "1");
  });
});
