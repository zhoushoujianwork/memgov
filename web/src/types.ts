export interface AgentWorkspace {
  id: string;
  kind: string;
  owner_principal_id?: string;
  channel_id?: string;
  conversation_id?: string;
  created_at: string;
}
export interface WorkspaceFile {
  path: string;
  digest: string;
  bytes: number;
  updated_at: string;
  snippet?: string;
}
export interface WorkspaceDocument extends WorkspaceFile {
  content: string;
}
export interface WorkspaceRevision {
  path: string;
  digest: string;
  previous_digest: string;
  actor: string;
  request_id: string;
  created_at: string;
  bytes: number;
}
export interface Task {
  id: string;
  version: number;
  title: string;
  preview?: string;
  result_summary?: string;
  status: string;
  work_phase?: string;
  work_deadline?: string;
  work_heartbeat?: string;
  model_activity_at?: string;
  mode: string;
  runtime_name: string;
  runtime_status: string;
  process_state: string;
  attention?: boolean;
  redacted?: boolean;
  updated_at: string;
  expires_at?: string;
  command: string;
}
export interface Attempt {
  id: string;
  task_version: number;
  status: string;
  model: string;
  started_at: string;
  finished_at?: string;
  applied_version: number;
  tools?: string[];
  summary?: string;
  error_code?: string;
  output_visible?: boolean;
  artifacts?: string[];
}
export interface TaskDetail {
  task: Task;
  can_resume: boolean;
  can_view_output: boolean;
  resume_mode: string;
  resume_reason?: string;
  resume_command: string;
  logs_command: string;
  result?: string;
  attempts: Attempt[];
  messages: {
    id: string;
    source_id?: string;
    sent_at: string;
    availability: string;
    body?: string;
  }[];
  deliveries: {
    id: string;
    state: string;
    updated_at: string;
    purpose?: string;
    transport?: string;
  }[];
  actions: {
    kind: string;
    status: string;
    target?: string;
    payload?: string;
    confirmation_token?: string;
  }[];
  communications?: {
    id: string;
    state: string;
    target_type: string;
    target_id: string;
    owner_user_id: string;
    owner_profile: string;
    reason?: string;
    content?: string;
    updated_at: string;
  }[];
}
export interface LogEvent {
  timestamp: string;
  component: string;
  event: string;
  error_code?: string;
}
export interface Runtime {
  work?: {
    analysis_active: number;
    execution_active: number;
    analysis_limit: number;
    execution_limit: number;
    queued_tasks: number;
    oldest_waiting_seconds: number;
    last_analysis_at: string;
    retry_batches: number;
    analysis_gaps: number;
    timed_out: number;
    analysis_health: string;
    task_health: string;
  };
  id: string;
  name: string;
  status: string;
  mode: string;
  process_state: string;
  tasks: Record<string, number>;
  pending_messages: number;
  waiting_receipt_messages?: number;
  pending_actions: number;
  config_version: number;
  runners?: {
    pid: number;
    heartbeat_at: string;
    version: string;
    build: string;
  }[];
}
export interface Meta {
  home: string;
  config_path: string;
  version: string;
  build: string;
  installed_build: string;
  database_schema: number;
  applied_version: number;
  agent_editing: boolean;
  service_restart: boolean;
  service?: {
    id: string;
    pid: number;
    state: string;
    heartbeat_at: string;
    modules: {
      key: string;
      name: string;
      state: string;
      error_code?: string;
    }[];
  };
}
export interface Watermark {
  conversation_id: string;
  covered_until: string;
  observed_at: string;
}
export interface SourceView {
  receiver_active: boolean;
  source: {
    id: string;
    name: string;
    status: string;
    direct_enabled: boolean;
    direct_enabled_at: string;
    retention_days: number;
    direct_discovery_covered_until: string;
    last_direct_received_at: string;
    last_retention_at: string;
    last_retention_count: number;
    last_error_code?: string;
    retention_error_code?: string;
  };
  coverage?: {
    conversations: (Watermark & {
      watermark?: Watermark;
      gaps?: { start_at: string; end_at: string; stop_reason: string }[];
    })[];
  };
}
export interface AgentView {
  runtime: string;
  conversation_id?: string;
  agent: string;
  preset: string;
  model: string;
  profile: string;
  bash: boolean;
  external_actions: string;
  inherit: string;
  capabilities?: string[];
  directories?: string[];
  policy_error?: string;
  skill_error?: string;
  skills: { name: string; origin: string; path: string; digest: string }[];
}
export interface AgentConfig {
  error?: string;
  revision: string;
  plan_command: string;
  items: {
    name: string;
    pending: boolean;
    references: string[];
    can_delete: boolean;
    agent: Record<string, unknown>;
  }[];
}
export interface Envelope<T> {
  data: T;
  sampled_at: string;
  ok: boolean;
  error?: { message: string };
}
