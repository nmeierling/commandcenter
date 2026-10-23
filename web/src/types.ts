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
export type PortState =
  | 'free'
  | 'owned'
  | 'conflict'
  | 'unknown-owner'
  | 'substituted'

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
  /** The port inside the container that this host port maps to. */
  containerPort?: number
  /** Compose environment variable that declares this port, if any. */
  variable?: string
  /** The originally declared port, set when the user moved this project. */
  declared?: number
}

export type PortChangeMechanism =
  | 'none'
  | 'env'
  | 'command'
  | 'compose-var'
  | 'compose-override'

/** Whether this project's host ports can be moved, and how. */
export interface PortControl {
  canChange: boolean
  mechanism: PortChangeMechanism
  /** Why not, when canChange is false. */
  reason?: string
}

export interface ConflictingPort {
  port: number
  /** The key an override is stored under — the project's declared port. */
  declared: number
  owner?: PortOwner
  ownerLabel: string
  canStopOwner: boolean
  suggested?: number
  service?: string
}

/** Returned with HTTP 409 when a start would collide with something. */
export interface PortConflict {
  projectId: string
  projectName: string
  ports: ConflictingPort[]
  portControl: PortControl
  /** Set when the conflict blocked one row rather than the whole project. */
  unitId?: string
  unitName?: string
  candidates?: SubstituteCandidate[]
  /** False when the dialog was opened deliberately rather than by a refusal. */
  blocked: boolean
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

export type UnitState =
  | 'running'
  | 'exited'
  | 'created'
  | 'not-created'
  | 'unknown'
  | 'substituted'

/** A unit that is deliberately not running because something else provides it. */
export interface Substitution {
  target: string
  projectId?: string
  projectName?: string
  unitId?: string
  satisfied: boolean
  detail?: string
  ports?: number[]
}

/** A running unit elsewhere that could stand in for this one. */
export interface SubstituteCandidate {
  projectId: string
  projectName: string
  unitId: string
  unitName: string
  target: string
  ports: number[]
  image?: string
  /** True when it already serves the exact port that is blocked. */
  matches: boolean
  /** 'project' for a unit devcc manages, 'docker' for an observed container. */
  source: 'project' | 'docker'
  container?: string
}

/** Pairs what a project asks for with what is actually published. */
export interface UnitPort {
  /** The port devcc will use: the override if set, else the declared value. */
  configured?: number
  /** The project's own value, present only when an override moved it. */
  declared?: number
  /** The port in use right now. Absent when nothing is listening. */
  actual?: number
  /** The port inside the container, for compose units. */
  container?: number
  state: PortState
  owner?: PortOwner
}

/** One runnable thing inside a project: a container, or a supervised app. */
export interface Unit {
  id: string
  name: string
  kind: 'container' | 'process'
  state: UnitState
  status?: string
  health?: string
  image?: string
  containerName?: string
  containerId?: string
  pid?: number
  running: boolean
  ports: UnitPort[]
  url?: string
  /** Why this unit cannot be started right now. */
  blocked?: string
  substitute?: Substitution
}

export interface ContainerPort {
  host: number
  container: number
  protocol?: string
}

export interface Container {
  name: string
  service: string
  state: string
  health?: string
  status?: string
  image?: string
  id?: string
  ports: ContainerPort[]
  /** Address to open for this container, empty when it publishes nothing. */
  url?: string
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
  units: Unit[]
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
  portControl: PortControl
  services?: string[]
  /** Checked-out git branch, or a short commit when detached. */
  branch?: string
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
