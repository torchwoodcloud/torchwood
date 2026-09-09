import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { HttpTransport } from "../http.js";
import { AccountService, parseOAuth2CallbackFragment } from "../client/account.js";
import { TorchwoodError } from "../errors.js";

function stubFetch(body: unknown, status = 200): typeof fetch {
  return async () =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json" },
    });
}

describe("AccountService MFA 分支", () => {
  it("signIn mfa_required 时返回 challenge_token 且不保存 token", async () => {
    const http = new HttpTransport({
      endpoint: "http://localhost:9080",
      projectId: "default",
      fetch: stubFetch({
        mfa_required: true,
        challenge_token: "ch-1",
        factors: [{ id: "f1", type: "totp", status: "verified" }],
      }),
    });
    const account = new AccountService(http);

    const res = await account.signIn({ email: "u@example.com", password: "pw" });
    assert.equal(res.mfa_required, true);
    assert.equal(res.challenge_token, "ch-1");
    assert.equal(res.factors?.[0]?.type, "totp");
    assert.equal(http.getAccessToken(), undefined);
  });

  it("signUp 成功分支保存 access token", async () => {
    const http = new HttpTransport({
      endpoint: "http://localhost:9080",
      projectId: "default",
      fetch: stubFetch({
        account: { id: "u1" },
        tokens: { access_token: "at-1", refresh_token: "rt-1", expires_at: "2026-08-13T06:00:00Z" },
      }),
    });
    const account = new AccountService(http);

    const res = await account.signUp({ email: "u@example.com", password: "pw", name: "User" });
    assert.equal(http.getAccessToken(), "at-1");
    assert.equal(res.tokens?.access_token, "at-1");
    assert.equal(res.tokens?.expires_at, "2026-08-13T06:00:00Z");
  });
});

describe("AccountService.deleteSessions", () => {
  it("keep_current 作为查询参数传递", async () => {
    const calls: string[] = [];
    const fetchImpl = async (input: RequestInfo | URL) => {
      calls.push(String(input));
      return new Response(null, { status: 204 });
    };
    const http = new HttpTransport({
      endpoint: "http://localhost:9080",
      projectId: "default",
      fetch: fetchImpl,
    });
    const account = new AccountService(http);

    await account.deleteSessions(true);
    assert.equal(calls.length, 1);
    const url = new URL(calls[0]);
    assert.equal(url.pathname, "/v1/account/sessions");
    assert.equal(url.searchParams.get("keep_current"), "true");
  });
});

describe("AccountService.deleteFactor", () => {
  it("携带 code 时经 query 传递；未携带时无 query", async () => {
    const calls: string[] = [];
    const fetchImpl = async (input: RequestInfo | URL) => {
      calls.push(String(input));
      return new Response(null, { status: 204 });
    };
    const http = new HttpTransport({
      endpoint: "http://localhost:9080",
      projectId: "default",
      fetch: fetchImpl,
    });
    const account = new AccountService(http);

    await account.deleteFactor("f1", "123456");
    assert.equal(calls.length, 1);
    let url = new URL(calls[0]);
    assert.equal(url.pathname, "/v1/account/mfa/f1");
    assert.equal(url.searchParams.get("code"), "123456");

    await account.deleteFactor("f2");
    assert.equal(calls.length, 2);
    url = new URL(calls[1]);
    assert.equal(url.pathname, "/v1/account/mfa/f2");
    assert.equal(url.searchParams.has("code"), false);
  });
});

describe("AccountService.buildOAuth2AuthorizeURL", () => {
  const http = new HttpTransport({
    endpoint: "http://localhost:9080/",
    projectId: "default",
    fetch: stubFetch({}),
  });
  const account = new AccountService(http);

  it("拼接 authorize URL：尾斜杠归一 + 三必填 query", () => {
    const url = new URL(
      account.buildOAuth2AuthorizeURL({
        provider: "github",
        success: "https://app.example.com/oauth/callback",
        failure: "https://app.example.com/login?error=oauth",
      })
    );
    assert.equal(
      url.origin + url.pathname,
      "http://localhost:9080/v1/account/oauth2/github/authorize"
    );
    assert.equal(url.searchParams.get("project_id"), "default");
    assert.equal(url.searchParams.get("success"), "https://app.example.com/oauth/callback");
    assert.equal(url.searchParams.get("failure"), "https://app.example.com/login?error=oauth");
  });

  it("provider 经 URI 编码，不产生双斜杠", () => {
    const url = new URL(
      account.buildOAuth2AuthorizeURL({
        provider: "wechat web",
        success: "https://app.example.com/cb",
        failure: "https://app.example.com/login",
      })
    );
    assert.equal(url.pathname, "/v1/account/oauth2/wechat%20web/authorize");
  });
});

describe("parseOAuth2CallbackFragment", () => {
  it("signed_in 形态：access_token + userId", () => {
    assert.deepEqual(parseOAuth2CallbackFragment("#access_token=jwt-1&userId=u1"), {
      type: "signed_in",
      accessToken: "jwt-1",
      userId: "u1",
    });
  });

  it("mfa_required 形态：challengeToken + 逗号分隔 factorTypes", () => {
    assert.deepEqual(
      parseOAuth2CallbackFragment(
        "#userId=u1&mfaRequired=true&challengeToken=ch-1&mfaFactorTypes=totp%2Cwebauthn"
      ),
      {
        type: "mfa_required",
        userId: "u1",
        challengeToken: "ch-1",
        factorTypes: ["totp", "webauthn"],
      }
    );
  });

  it("非回调 fragment 返回 null", () => {
    assert.equal(parseOAuth2CallbackFragment(""), null);
    assert.equal(parseOAuth2CallbackFragment("#utm_source=twitter"), null);
  });

  it("声称回调但缺必填字段抛 TorchwoodError", () => {
    const expectThrow = (fragment: string, match: RegExp) => {
      try {
        parseOAuth2CallbackFragment(fragment);
      } catch (err) {
        assert.ok(err instanceof TorchwoodError, "expected TorchwoodError");
        const message = err instanceof TorchwoodError ? err.message : "";
        assert.ok(match.test(message), `message ${message} 应匹配 ${match}`);
        return;
      }
      assert.ok(false, `expected throw for ${fragment}`);
    };
    expectThrow("#access_token=jwt-1", /userId/);
    expectThrow("#userId=u1&mfaRequired=true", /challengeToken/);
  });
});
