export type ID = string;
export type AccessLevel = 'none' | 'view' | 'edit';
export type Result<T> = { data: T };
export type List<T> = { items: T[] };

export type ApiError = {
  error: {
    code: string;
    message: string;
    field_errors: Record<string, string>;
    retryable: boolean;
  };
};

export type User = { 
  id: ID; 
  username: string; 
  is_admin: boolean; 
  is_active: boolean 
};

export type SessionView = { 
  user: User; 
  absolute_expires_at: string 
};

export type CsrfView = { csrf_token: string };

export type Group = { 
  id: ID; 
  name: string 
};

export type GroupMembers = { 
  group_id: ID; 
  user_ids: ID[] 
};

export type GroupGrants = {
  group_id: ID;
  grants: { 
    database_id: ID; 
    access: 'view' | 'edit' 
  }[];
};

export type DatabaseSummary = {
  id: ID; 
  name: string; 
  access: 'view' | 'edit';
  execution_ready?: boolean;
};

export type ConnectionSummary = {
  id: ID; 
  name: string; 
  host: string; 
  port: number;
  database_name: string; 
  username: string; 
  ssl_mode: 'disable' | 'require' | 'verify-full';
  executor_username?: string | null;
  preparer_username?: string | null;
  execution_ready?: boolean;
};

export type ConnectionCredential = { username: string; password: string };

export type ConnectionInput = {
  name: string; 
  host: string; 
  port: number; 
  database_name: string;
  username: string; 
  password: string; 
  ssl_mode: 'disable' | 'require' | 'verify-full';
  executor?: ConnectionCredential;
  preparer?: ConnectionCredential;
};

export type SchemaSummary = { 
  name: string; 
  tables: { 
    name: string 
  }[] 
};

export type Column = {
  name: string; 
  data_type: string; 
  nullable: boolean; 
  default_expression: string | null;
};

export type Constraint = { 
  name: string; 
  kind: string; 
  definition: string 
};

export type Index = { 
  name: string; 
  definition: string; 
  is_unique: boolean; 
  is_primary: boolean 
};

export type TableMetadata = {
  schema: string; 
  table: string; 
  columns: Column[];
  constraints: Constraint[]; 
  indexes: Index[];
};

export type TableRows = {
  columns: { 
    name: string; 
    data_type: string 
  }[];
  rows: (string | null)[][];
  limit: number; 
  offset: number; 
  has_more: boolean;
};

export type Draft = {
  id: ID; 
  database_id: ID; 
  title: string; 
  sql: string;
  revision: number; 
  created_by: ID; 
  updated_at: string;
  applied_execution_id: ID | null;
  sql_down?: string | null;
  irreversible_reason?: string | null;
  mode?: MigrationMode;
  batch?: BatchConfig | null;
};

export type ExecutionStatus = 'running' | 'succeeded' | 'failed' | 'unknown';

export type Execution = {
  id: ID; 
  database_id: ID; 
  draft_id: ID; 
  draft_revision: number;
  version: string; 
  title: string; 
  sql: string;

  executed_by: { 
    id: ID; 
    username: string 
  };

  started_at: string; 
  finished_at: string | null;
  status: ExecutionStatus;

  error: { 
    code: string; 
    message: string 
  } | null;
};

export type ApiErrorBody = ApiError['error'];
export type LoginInput = { 
  username: string; 
  password: string 
};

export type LogoutResult = { logged_out: true };
export type DeletedResult = { deleted: true };
export type UserCreateInput = { 
  username: string; 
  password: string; 
  is_admin: boolean 
};

export type UserPatchInput = {
  username?: string; 
  password?: string; 
  is_admin?: boolean; 
  is_active?: boolean;
};

export type GroupInput = { name: string };
export type GroupMembersInput = { user_ids: ID[] };
export type GroupGrantsInput = { 
  grants: { 
    database_id: ID; 
    access: 'view' | 'edit' 
  }[] 
};

export type ConnectionCredentialPatch = { username: string; password?: string };
export type ConnectionPatchInput = Partial<Omit<ConnectionInput, 'executor' | 'preparer'>> & {
  executor?: ConnectionCredentialPatch;
  preparer?: ConnectionCredentialPatch;
};

export type ConnectionTestResult = { reachable: true };
export type RowsQuery = { 
  schema: string; 
  table: string; 
  limit: number; 
  offset: number 
};

export type DraftCreateInput = { 
  title: string; 
  sql: string 
};

export type DraftPatchInput = { 
  title: string; 
  sql: string; 
  revision: number 
};

export type ExecuteInput = { 
  revision: number; 
  request_id: string 
};

export type DecimalString = string;
export type JsonObject = Record<string, unknown>;

export type MigrationAction = 'apply' | 'rollback' | 'forward' | 'resume' | 'compensate';
export type Direction = 'up' | 'down';
export type MigrationMode = 'standard' | 'batch';
export type ProposalStatus = 'draft' | 'pending_review' | 'approved' | 'queued' | 'superseded';
export type ValidationStatus = 'passed' | 'failed';
export type ApprovalStatus = 'pending' | 'approved';
export type JobStatus =
  | 'queued'
  | 'running'
  | 'retry_wait'
  | 'succeeded'
  | 'failed'
  | 'partial'
  | 'partially_reverted'
  | 'unknown';
export type StatementOutcome = 'succeeded' | 'failed' | 'unknown';

export type AllowedAction =
  | 'submit_review'
  | 'approve'
  | 'enqueue'
  | 'propose_apply'
  | 'propose_rollback'
  | 'propose_forward'
  | 'propose_resume'
  | 'propose_compensate'
  | 'edit'
  | 'discard';

export type Success<T> = { success: true; data: T; error: null };
export type Failure = { success: false; data: null; error: ApiErrorBody };

export type CursorPage<T> = { items: T[]; next_cursor: string | null };
export type Actor = { id: ID; username: string };

export type BatchConfig = {
  schema: string;
  table: string;
  cursor_column: string;
  // 1–10000
  batch_size: number;
  parameters: JsonObject;
};

export type Snapshot = {
  migration_id: ID;
  database_id: ID;
  title: string;
  sql_up: string;
  sql_down: string | null;
  irreversible_reason: string | null;
  revision: number;
  mode: MigrationMode;
  batch: BatchConfig | null;
};

export type Validation = {
  status: ValidationStatus;
  checked_at: string;
  issues: { code: string; message: string }[];
};

export type Approval = {
  group_id: ID;
  leader_user_id: ID | null;
  status: ApprovalStatus;
  approved_at: string | null;
};

export type Proposal = {
  proposal_id: ID;
  database_id: ID;
  migration_id: ID;
  revision: number;
  action: MigrationAction;
  source_execution_id: ID | null;
  status: ProposalStatus;
  snapshot: Snapshot;
  validation: Validation | null;
  approvals: Approval[];
  allowed_actions: AllowedAction[];
};

export type Progress = {
  direction: Direction;
  batches_committed: DecimalString;
  rows_committed: DecimalString;
  batches_reverted: DecimalString;
  rows_reverted: DecimalString;
  total_rows: DecimalString | null;
};

export type JobError = { code: string; message: string; retryable: boolean };

export type JobAccepted = {
  job_id: ID;
  execution_id: ID;
  proposal_id: ID;
  database_id: ID;
  status: JobStatus;
  status_url: string;
};

export type JobView = JobAccepted & {
  migration_id: ID;
  revision: number;
  action: MigrationAction;
  mode: MigrationMode;
  execution_status: JobStatus;
  attempt_no: number;
  blocks_database: boolean;
  progress: Progress | null;
  source_execution_id: ID | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  next_retry_at: string | null;
  error: JobError | null;
  allowed_actions: AllowedAction[];
};

export type Attempt = {
  attempt_id: ID;
  job_id: ID | null;
  execution_id: ID;
  database_id: ID;
  migration_id: ID;
  revision: number;
  actor_id: ID;
  attempt_no: number;
  status: string;
  started_at: string;
  finished_at: string | null;
  error: JobError | null;
  legacy_execution_id: ID | null;
};

export type BatchCommit = {
  batch_index: DecimalString;
  row_count: DecimalString;
  committed_at: string;
  receipt: string;
  range_start: string | null;
  range_end: string | null;
  sql_snapshot: string;
  safe_parameters: JsonObject;
  job_id: ID;
  executed_by: Actor;
};

export type HistoryEntry = {
  execution_id: ID;
  migration_id: ID;
  revision: number;
  version: DecimalString;
  action: MigrationAction;
  direction: Direction;
  executed_by: Actor;
  committed_at: string;
  sql_snapshot: string;
  source_execution_id: ID | null;
  reverts_execution_id: ID | null;
  is_complete: boolean;
  batch_commits: BatchCommit[];
  id: ID;
  database_id: ID;
  draft_id: ID;
  draft_revision: number;
  title: string;
  sql: string;
  started_at: string;
  finished_at: string | null;
  status: string;
  error: { code: string; message: string } | null;
};

export type LegacyHistory = {
  version: DecimalString;
  executed_by: Actor;
  id: ID;
  database_id: ID;
  draft_id: ID;
  draft_revision: number;
  title: string;
  sql: string;
  started_at: string;
  finished_at: string | null;
  status: string;
  error: { code: string; message: string } | null;
};

export type HistoryStatement = {
  ordinal: number;
  batch_index: number | null;
  sql: string;
  parameters: JsonObject;
  outcome: StatementOutcome;
};

export type HistoryAttemptRow = {
  attempt_id: ID;
  execution_id: ID;
  migration_id: ID;
  revision: number | null;
  version: DecimalString | null;
  actor: Actor | null;
  action: MigrationAction;
  direction: Direction;
  status: string;
  started_at: string;
  finished_at: string | null;
  source_execution_id: ID | null;
  sql_entered: boolean | null;
  statements: HistoryStatement[];
  commit: HistoryEntry | null;
};

export type DraftInputM2 = {
  title: string;
  sql: string;
  sql_down: string | null;
  irreversible_reason: string | null;
  mode: MigrationMode;
  batch: BatchConfig | null;
};

export type DraftUpdateM2 = DraftInputM2 & { revision: number };

export type ProposalCreate = {
  migration_id: ID;
  revision: number;
  action: MigrationAction;
  source_execution_id: ID | null;
};

export type RevisionRequest = { revision: number };
export type ApprovalRequest = { revision: number; group_id: ID };
export type EnqueueRequest = { request_id: string; revision: number };
