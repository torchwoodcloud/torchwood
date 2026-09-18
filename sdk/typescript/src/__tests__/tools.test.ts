import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  TOOL_CREATE_DOCUMENT,
  TOOL_CREATE_USER,
  TOOL_DELETE_DOCUMENT,
  TOOL_GET_COLLECTION,
  TOOL_GET_DOCUMENT,
  TOOL_GET_FILE,
  TOOL_GET_HEALTH,
  TOOL_GET_ORDER,
  TOOL_GET_USER,
  TOOL_GRANT_ASSET,
  TOOL_INVOKE_FUNCTION,
  TOOL_LIST_COLLECTIONS,
  TOOL_LIST_FILES,
  TOOL_LIST_USER_ASSETS,
  TOOL_LIST_USERS,
  TOOL_QUERY_DOCUMENTS,
  TOOL_UPDATE_DOCUMENT,
  TOOL_UPSERT_DOCUMENT,
  agentTools,
  lookupAgentTool,
} from "../index.js";

const GENPROTO_SERVER_DIR = join(
  dirname(fileURLToPath(import.meta.url)),
  "../../../../genproto/server/v1"
);

function serverSwaggerOperationIds(): Set<string> {
  const ids = new Set<string>();
  assert.ok(existsSync(GENPROTO_SERVER_DIR), "genproto/server/v1 缺失，请先 mise run generate:proto");
  for (const f of readdirSync(GENPROTO_SERVER_DIR).filter((n) => n.endsWith(".swagger.json"))) {
    const doc = JSON.parse(readFileSync(join(GENPROTO_SERVER_DIR, f), "utf8")) as {
      paths?: Record<string, Record<string, { operationId?: string }>>;
    };
    for (const path of Object.values(doc.paths ?? {})) {
      for (const op of Object.values(path)) {
        if (op?.operationId) ids.add(op.operationId);
      }
    }
  }
  return ids;
}

function swaggerHasRPC(opIds: Set<string>, service: string, rpc: string): boolean {
  if (opIds.has(`${service}_${rpc}`)) return true;
  const extra = new RegExp(`^${service}_${rpc}\\d+$`);
  for (const id of opIds) {
    if (extra.test(id)) return true;
  }
  return false;
}

describe("agentTools catalog", () => {
  it("maps 18 names to known FullMethod strings and excludes API key methods", () => {
    const want: Array<{ name: string; fullMethod: string }> = [
      { name: TOOL_LIST_USERS, fullMethod: "/torchwood.server.v1.UsersService/ListUsers" },
      { name: TOOL_GET_USER, fullMethod: "/torchwood.server.v1.UsersService/GetUser" },
      { name: TOOL_CREATE_USER, fullMethod: "/torchwood.server.v1.UsersService/CreateUser" },
      {
        name: TOOL_QUERY_DOCUMENTS,
        fullMethod: "/torchwood.server.v1.DatabasesService/ListDocuments",
      },
      { name: TOOL_GET_DOCUMENT, fullMethod: "/torchwood.server.v1.DatabasesService/GetDocument" },
      {
        name: TOOL_CREATE_DOCUMENT,
        fullMethod: "/torchwood.server.v1.DatabasesService/CreateDocument",
      },
      {
        name: TOOL_UPDATE_DOCUMENT,
        fullMethod: "/torchwood.server.v1.DatabasesService/UpdateDocument",
      },
      {
        name: TOOL_UPSERT_DOCUMENT,
        fullMethod: "/torchwood.server.v1.DatabasesService/UpsertDocument",
      },
      {
        name: TOOL_DELETE_DOCUMENT,
        fullMethod: "/torchwood.server.v1.DatabasesService/DeleteDocument",
      },
      {
        name: TOOL_LIST_COLLECTIONS,
        fullMethod: "/torchwood.server.v1.DatabasesService/ListCollections",
      },
      {
        name: TOOL_GET_COLLECTION,
        fullMethod: "/torchwood.server.v1.DatabasesService/GetCollection",
      },
      {
        name: TOOL_INVOKE_FUNCTION,
        fullMethod: "/torchwood.server.v1.FunctionsService/CreateExecution",
      },
      { name: TOOL_LIST_FILES, fullMethod: "/torchwood.server.v1.StorageService/ListFiles" },
      { name: TOOL_GET_FILE, fullMethod: "/torchwood.server.v1.StorageService/GetFile" },
      { name: TOOL_GRANT_ASSET, fullMethod: "/torchwood.server.v1.AssetsService/Grant" },
      {
        name: TOOL_LIST_USER_ASSETS,
        fullMethod: "/torchwood.server.v1.AssetsService/ListUserAssets",
      },
      { name: TOOL_GET_ORDER, fullMethod: "/torchwood.server.v1.PaymentsService/GetOrder" },
      { name: TOOL_GET_HEALTH, fullMethod: "/torchwood.server.v1.HealthService/Check" },
    ];

    assert.equal(want.length, 18);
    assert.equal(agentTools.length, 18);

    const opIds = serverSwaggerOperationIds();
    const seen = new Set<string>();
    for (const [i, tool] of agentTools.entries()) {
      assert.equal(tool.name, want[i].name);
      assert.equal(tool.fullMethod, want[i].fullMethod);
      assert.equal(tool.fullMethod.includes("APIKeys"), false);
      const m = /^\/torchwood\.server\.v1\.(\w+)\/(\w+)$/.exec(tool.fullMethod);
      if (m === null) {
        throw new Error(`bad FullMethod ${tool.fullMethod}`);
      }
      assert.ok(
        swaggerHasRPC(opIds, m[1], m[2]),
        `${tool.fullMethod} 不在 genproto swagger（${m[1]}_${m[2]}）`
      );
      assert.deepEqual(lookupAgentTool(tool.name), tool);
      assert.equal(seen.has(tool.name), false);
      seen.add(tool.name);
    }

    for (const method of [
      "/torchwood.server.v1.APIKeysService/CreateAPIKey",
      "/torchwood.server.v1.APIKeysService/ListAPIKeys",
      "/torchwood.server.v1.APIKeysService/GetAPIKey",
      "/torchwood.server.v1.APIKeysService/DeleteAPIKey",
    ]) {
      assert.equal(
        agentTools.some((t) => t.fullMethod === method),
        false
      );
    }
    for (const name of ["create_api_key", "list_api_keys", "get_api_key", "delete_api_key"]) {
      assert.equal(lookupAgentTool(name), undefined);
    }
  });
});
