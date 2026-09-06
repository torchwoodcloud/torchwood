package auth

// 本文件曾是 apiKeyScopeRules 手写表的家（单一事实源已迁 proto method_auth，
// 由 runtime.BuildMethodPolicies 收集进 PolicySet——机制重设计 M2/M3）。
// 表本体、scope 匹配（PolicySet.AllowsAPIKey）、词表校验
// （ScopeVocabulary）与启动覆盖断言（AssertSemantic）均已迁移/退役。

// StorageServiceCreateFile is the gRPC method used for HTTP storage scope checks.
const StorageServiceCreateFile = "/torchwood.server.v1.StorageService/CreateFile"

// StorageServiceGetFile is the gRPC method used for HTTP storage read scope checks.
const StorageServiceGetFile = "/torchwood.server.v1.StorageService/GetFile"
