import io, re, sys, os

ROOT = r"D:/Codes/qiulin/torchwood"

def read(p):
    with io.open(os.path.join(ROOT, p), encoding="utf-8") as f:
        return f.read()

def write(p, s):
    with io.open(os.path.join(ROOT, p), "w", encoding="utf-8", newline="") as f:
        f.write(s)

def union_resolve(p, transforms=()):
    """取冲突块两侧的并集（先 HEAD 侧再分支侧），再应用 transforms。"""
    s = read(p)
    pat = re.compile(r"<<<<<<< HEAD\n(.*?)=======\n(.*?)>>>>>>> [^\n]*\n", re.S)
    def repl(m):
        ours, theirs = m.group(1), m.group(2)
        for t in transforms:
            ours, theirs = t(ours, theirs)
        return ours + theirs
    out, n = pat.subn(repl, s)
    if n == 0:
        print(f"WARN: no conflict blocks in {p}")
    write(p, out)
    print(f"resolved {p} ({n} blocks)")

# 1) authz.proto：并集 + analytics 重编号 15
union_resolve(r"proto/shared/v1/authz.proto", transforms=[
    lambda o, t: (o, t.replace("SCOPE_RESOURCE_ANALYTICS = 14;", "SCOPE_RESOURCE_ANALYTICS = 15;"))
])

# 2) 生成物 authz.pb.go：先取任一侧（后面 regen 覆盖）
union_resolve(r"genproto/shared/v1/authz.pb.go")

# 3) policy.go / authz_policy.go / grpc.go / gateway / api provides / worker providers / Layout / README：纯并集
for p in [
    r"internal/domain/auth/policy.go",
    r"cmd/server/internal/runtime/authz_policy.go",
    r"cmd/server/internal/runtime/grpc.go",
    r"cmd/server/internal/runtime/grpc_gateway.go",
    r"internal/api/provides.go",
    r"worker/providers.go",
    r"console/src/components/Layout.tsx",
    r"docs/developer/README.md",
    r"docs/developer/authz-matrix.md",
    r"sdk/go/server/scopes.go",
]:
    union_resolve(p)

# 4) wire_gen ×2：取 HEAD 侧（后面 task wire:all 重生成，分支侧 provider 由 provides 决定）
for p in [r"cmd/server/wire_gen.go", r"cmd/worker/wire_gen.go"]:
    s = read(p)
    pat = re.compile(r"<<<<<<< HEAD\n(.*?)=======\n(.*?)>>>>>>> [^\n]*\n", re.S)
    out, n = pat.subn(lambda m: m.group(1), s)
    write(p, out)
    print(f"resolved {p} (HEAD side, {n} blocks; wire regen follows)")

# 5) cmd/worker/provides.go 冲突（NewComponents / ProviderSet 两处）纯并集
union_resolve(r"cmd/worker/provides.go")

# 6) invoke_test.go：手工看块（计数行双方都改了 129）
print("--- invoke_test conflict ---")
s = read(r"sdk/go/server/invoke_test.go")
for m in re.finditer(r"<<<<<<< HEAD\n(.*?)=======\n(.*?)>>>>>>> [^\n]*\n", s, re.S):
    print("OURS:", m.group(1)[:300])
    print("THEIRS:", m.group(2)[:300])

# 7) contract.test.ts：手工看块
print("--- contract.test conflict ---")
s = read(r"sdk/typescript/src/__tests__/contract.test.ts")
for m in re.finditer(r"<<<<<<< HEAD\n(.*?)=======\n(.*?)>>>>>>> [^\n]*\n", s, re.S):
    print("OURS:", m.group(1)[:300])
    print("THEIRS:", m.group(2)[:300])
