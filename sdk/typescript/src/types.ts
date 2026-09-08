export interface ListMeta {
  page_size?: number;
  total_count?: number;
  next_page_token?: string;
}

export interface ListParams {
  queries?: string[];
  page_size?: number;
  page_token?: string;
}

// 文档面（ListDocuments/CountDocuments）查询参数（C7 单 AST）：过滤/排序/
// 投影走 query AST；page_size/page_token 保留为简单分页参数（GET 面）。
// 其余面（assets/groups/…）的 queries DSL 不受影响。
export interface DocumentListParams {
  query?: import("./query.js").QueryAst;
  page_size?: number;
  page_token?: string;
}

export interface Account {
  id: string;
  email: string;
  name: string;
  status: string;
  email_verified: boolean;
  created_at: string;
  updated_at: string;
}

export interface TokenBundle {
  access_token: string;
  refresh_token: string;
  // protojson 将 int64 序列化为字符串（如 "1800000"）；兼容旧 number 响应。
  expires_at: string;
}

// SignUp/SignIn 等认证响应；mfa_required=true 时服务端不返回 tokens，
// 只返回 challenge_token 与 factors 引导二次认证（调用方不得在该分支访问 tokens）。
export interface AuthResult {
  account: Account;
  // 仅当 mfa_required 为 false 时存在；mfa_required=true 时无 tokens，访问前必须先判空。
  tokens?: TokenBundle;
  mfa_required?: boolean;
  challenge_token?: string;
  factors?: Factor[];
}

export interface Factor {
  id: string;
  type: string;
  status: string;
  created_at?: string;
}

export interface Session {
  id: string;
  user_id: string;
  provider: string;
  user_agent: string;
  ip: string;
  expire_at: string;
  created_at: string;
  current: boolean;
}

export interface Document {
  id: string;
  data: Record<string, unknown>;
  permissions?: string[];
  created_at: string;
  updated_at: string;
  // 用户集合 OCC 版本；int64，网关可能给 string，消费时 Number()。
  version?: number;
}

/** 已提交的文档写事件（阶段④ §4.5 补偿 API；delete 事件无 data = tombstone）。 */
export interface Change {
  seq: number | string;
  event_id: string;
  event: string;
  document_id: string;
  // int64，网关可能给 string，消费时 Number()。
  version: number | string;
  created_at: string;
  truncated?: boolean;
  data?: Document;
  // 非空表示来自 execute-tx 原子批（批内顺序 = op 序）。
  transaction_id?: string;
}

/** ListChanges 响应：next_since_seq 是续传游标（R15 两级语义——满页 = 末条
 * 返回 seq；扫描触顶 = 越过不可见块的扫描位置），续传**优先使用本字段**，
 * 仅当为 0 时回退末条 change 的 seq；has_more=false 时恒为 0。 */
export interface ListChangesResponse {
  changes?: Change[];
  has_more?: boolean;
  next_since_seq?: number | string;
}

export interface Group {
  id: string;
  name: string;
  total: number;
  permissions?: string[];
  created_at: string;
  updated_at: string;
}

export interface Membership {
  id: string;
  group_id: string;
  user_id: string;
  email: string;
  name: string;
  roles: string[];
  status: string;
  invited_at?: string;
  joined_at?: string;
  created_at: string;
  updated_at: string;
}

export interface Project {
  id: string;
  name: string;
  description?: string;
  status: string;
  registration_policy?: string;
  oauth_allowed_redirect_urls?: string[];
  created_at: string;
  updated_at: string;
}

export interface InviteCode {
  id: string;
  project_id: string;
  code: string;
  max_uses: number;
  used_count: number;
  expire_at?: string;
  revoked: boolean;
  created_by: string;
  created_at: string;
}

export interface APIKey {
  id: string;
  name: string;
  scopes: string[];
  enabled: boolean;
  expire_at?: string;
  created_at: string;
  updated_at: string;
}

export interface User {
  id: string;
  email: string;
  name: string;
  status: string;
  email_verified: boolean;
  // proto 为 Struct：直接透传对象（勿用 {values:[...]} 包装）。
  labels?: Record<string, unknown>;
  prefs?: Record<string, unknown>;
  phone?: string;
  created_at: string;
  updated_at: string;
}

export interface Database {
  id: string;
  name: string;
  created_at: string;
  updated_at: string;
}

export interface Attribute {
  id: string;
  key: string;
  type: string;
  size?: number;
  required: boolean;
  array: boolean;
  default_value?: string;
  /** 生命周期状态（B4，§4.6）：active（缺省省略）| migrating | deprecated | retired。 */
  status?: string;
}

/** copy 迁移任务读回（B4；物理列名不出 API 契约）。 */
export interface AttributeMigration {
  id: string;
  key: string;
  phase: string;
  rows_done: string | number;
  schema_version: string | number;
}

export interface Index {
  id: string;
  type: string;
  attributes: string[];
  orders: string[];
}

export interface Collection {
  id: string;
  database_id: string;
  name: string;
  permissions: string[];
  attributes: Attribute[];
  indexes: Index[];
  document_security?: boolean;
  disabled?: boolean;
  is_system?: boolean;
  created_at: string;
  updated_at: string;
}

export interface BulkDocumentsResponse {
  // int64：网关序列化为字符串。
  affected: string | number;
}

export interface UpdateDocumentInput {
  data?: Record<string, unknown>;
  permissions?: string[];
  increment?: Record<string, number>;
  // 用户集合 OCC 必填：来自上一次 GetDocument/List 的 version。
  version: number;
}

export interface UpsertDocumentInput {
  data: Record<string, unknown>;
  permissions?: string[];
  conflict_columns: string[];
}

export interface Bucket {
  id: string;
  name: string;
  permissions: string[];
  public?: boolean;
  created_at: string;
  updated_at: string;
}

export interface FileItem {
  id: string;
  bucket_id: string;
  name: string;
  mime_type: string;
  // int64：网关序列化为字符串。
  size: string | number;
  metadata?: Record<string, string>;
  created_at: string;
  updated_at: string;
}

export interface FileToken {
  // 仅创建响应返回一次，之后任何接口不回显。
  token: string;
  expires_at: string;
}

export interface StorageUsage {
  buckets: number;
  files: number;
  // int64：网关序列化为字符串。
  total_size: string | number;
}

export interface FunctionInfo {
  id: string;
  project_id: string;
  name: string;
  runtime: string;
  entrypoint: string;
  timeout_seconds: number;
  spec: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface RuntimeInfo {
  id: string;
  name: string;
  entrypoint: string;
}

export interface SpecificationInfo {
  id: string;
  cpu: string;
  memory: string;
}

export interface Deployment {
  id: string;
  function_id: string;
  // int64：网关序列化为字符串。
  size: string | number;
  status: string;
  error?: string;
  created_at: string;
  updated_at: string;
}

export interface Variable {
  key: string;
  // 敏感数据：调用方不得持久化/日志输出明文。
  value: string;
}

export interface Execution {
  id: string;
  function_id: string;
  deployment_id: string;
  status: string;
  response?: string;
  stdout?: string;
  stderr?: string;
  status_code?: number;
  // int64：网关序列化为字符串。
  duration_ms: string | number;
  error?: string;
  response_truncated?: boolean;
  stdout_truncated?: boolean;
  stderr_truncated?: boolean;
  created_at: string;
  updated_at: string;
}

export interface LogEntry {
  id: string;
  action: string;
  status: string;
  resource_id?: string;
  ip: string;
  user_agent: string;
  created_at: string;
}

export interface TOTPFactor {
  factor: Factor;
  // 仅创建响应返回明文 secret（otpauth_url 亦含 secret），之后不回显。
  secret?: string;
  otpauth_url?: string;
}

/** int64 最小单位；网关 protojson 序列化为字符串。禁止按 number 做算术。 */
export type Int64String = string;

export interface PaymentOrder {
  id: string;
  project_id?: string;
  user_id?: string;
  provider: string;
  amount: Int64String;
  currency: string;
  purpose_kind: string;
  purpose?: Record<string, unknown>;
  status: string;
  idempotency_key?: string;
  provider_session_id?: string;
  provider_order_id?: string;
  created_at?: string;
  paid_at?: string;
  expires_at?: string;
  payment_url?: string;
}

export interface CreateOrderResponse {
  order: PaymentOrder;
  idempotent_replay?: boolean;
}

export interface PaymentFulfillment {
  id: string;
  order_id: string;
  purpose_kind: string;
  ref: string;
  status: string;
  detail?: Record<string, unknown>;
  created_at?: string;
  updated_at?: string;
}

export interface AssetDef {
  id: string;
  project_id?: string;
  code: string;
  name: string;
  class: string;
  decimals: number;
  max_quantity?: Int64String;
  expires_in?: Int64String;
  tradable?: boolean;
  unique_per_owner?: boolean;
  upgradeable?: boolean;
  metadata?: Record<string, unknown>;
  status?: string;
  created_at?: string;
  updated_at?: string;
}

export interface AssetHolding {
  id: string;
  owner_id?: string;
  def_id: string;
  def_code: string;
  class: string;
  quantity: Int64String;
  expires_at?: string;
  level?: number;
  metadata?: Record<string, unknown>;
}

export interface AssetLedgerEntry {
  id: string;
  holding_id?: string;
  owner_id?: string;
  def_id: string;
  def_code?: string;
  kind: string;
  delta: Int64String;
  quantity_after: Int64String;
  expires_at?: string;
  ref_type?: string;
  ref_id?: string;
  idempotency_key?: string;
  created_at?: string;
}

export interface AssetOpResponse {
  entries: AssetLedgerEntry[];
  idempotent_replay?: boolean;
}

export interface AssetDrift {
  owner_id: string;
  def_id: string;
  holding_qty: Int64String;
  replayed_qty: Int64String;
  detail?: string;
}

export interface ReconcileResponse {
  zero_drift: boolean;
  holdings: number;
  entries: number;
  drift_count: number;
  drifts?: AssetDrift[];
}

export interface BenefitGrant {
  asset_code: string;
  quantity: Int64String;
  expires_in?: Int64String;
}

export interface BenefitEntitlement {
  asset_code: string;
  tier: number;
}

export interface Benefits {
  grants?: BenefitGrant[];
  entitlements?: BenefitEntitlement[];
}

export interface ProviderOverrides {
  stripe_price_id?: string;
}

export interface SubscriptionPlan {
  id: string;
  project_id?: string;
  code: string;
  name: string;
  amount: Int64String;
  currency: string;
  interval: string;
  interval_days?: Int64String;
  grace_days?: number;
  trial_days?: number;
  benefits?: Benefits;
  provider_overrides?: ProviderOverrides;
  status?: string;
  created_at?: string;
  updated_at?: string;
}

export interface Subscription {
  id: string;
  project_id?: string;
  user_id?: string;
  plan_id?: string;
  plan_code?: string;
  mode: string;
  provider?: string;
  provider_sub_id?: string;
  status: string;
  current_period_start?: string;
  current_period_end?: string;
  cancel_at_period_end?: boolean;
  grace_until?: string;
  billing_asset_code?: string;
  benefits?: Benefits;
  created_at?: string;
  updated_at?: string;
  payment_url?: string;
}

export interface SubscribeResponse {
  subscription: Subscription;
  idempotent_replay?: boolean;
  payment_url?: string;
  order_id?: string;
}

export interface ManualFulfillResponse {
  order: PaymentOrder;
  fulfillment: PaymentFulfillment;
}
