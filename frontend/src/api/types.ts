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
};

export type ConnectionSummary = {
  id: ID; 
  name: string; 
  host: string; 
  port: number;
  database_name: string; 
  username: string; 
  ssl_mode: 'disable' | 'require' | 'verify-full';
};

export type ConnectionInput = {
  name: string; 
  host: string; 
  port: number; 
  database_name: string;
  username: string; 
  password: string; 
  ssl_mode: 'disable' | 'require' | 'verify-full';
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

export type ConnectionPatchInput = Partial<ConnectionInput>;
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
