// Mirrors the Go types in agent/internal/model. Kept hand-written rather than
// generated: the surface is small, and a mismatch shows up immediately in the
// dashboard rather than in a build step.

export type Status =
  | 'running'
  | 'stopped'
  | 'starting'
  | 'stopping'
  | 'unhealthy'
  | 'conflict'
  | 'error'
  | 'unknown'

export type Kind = 'compose' | 'node' | 'go' | 'rust' | 'python' | 'custom'
export type Runtime = 'compose' | 'process' | 'none'
export type PortState = 'free' | 'owned' | 'conflict' | 'unknown-owner'

export interface PortOwner {
  pid?: number
  process?: string
  command?: string
  containerId?: string
  container?: string
  composeProject?: string
}

export interface Port {
  port: number
  source: string
  state: PortState
  owner?: PortOwner
  service?: string
}

export interface HealthCheck {
  url: string
  expectStatus?: number
  timeoutSeconds?: number
}

export interface HealthResult {
  checked: boolean
  ok: boolean
  statusCode?: number
  error?: string
  latencyMs?: number
  at?: string
}

export interface Container {
  name: string
  service: string
  state: string
  health?: string
  status?: string
  image?: string
  id?: string
}

export interface ProcessInfo {
  pid: number
  command: string
  startedAt: string
  running: boolean
  exitCode?: number
  exitError?: string
  stopRequested?: boolean
}

export interface State {
  status: Status
  detail?: string
  ports: Port[]
  containers: Container[]
  process?: ProcessInfo
  health: HealthResult
  busy?: string
  error?: string
  updatedAt: string
}

export interface Project {
  id: string
  name: string
  path: string
  kind: Kind
  markers?: string[]
  runtime: Runtime
  source: string
  startCommand?: string
  stopCommand?: string
  composeFile?: string
  composeProject?: string
  ports: Port[]
  urls: string[]
  health?: HealthCheck
  disabled?: boolean
  state: State
}

export interface AgentStatus {
  version: string
  dockerOk: boolean
  dockerVersion?: string
  dockerError?: string
  daemonOk: boolean
  canOpenBrowser: boolean
  portOwnersOk: boolean
  configPath?: string
  scanRoots?: string[]
  missingRoots?: string[]
  platform: string
  projectCount: number
}

export interface LogLine {
  seq?: number
  stream: string
  text: string
  at?: string
}
