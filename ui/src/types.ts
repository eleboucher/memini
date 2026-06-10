// Mirrors the JSON shapes returned by memini's REST API (internal/api/rest).

export type Tier = 'working' | 'episodic' | 'semantic' | 'procedural'

export const TIERS: Tier[] = ['working', 'episodic', 'semantic', 'procedural']

export interface Memory {
  id: string
  namespace: string
  tier: Tier
  content: string
  summary?: string
  metadata?: Record<string, unknown>
  tags?: string[]
  importance: number
  created_at: string
  updated_at: string
  last_accessed_at: string
  access_count: number
  expires_at?: string
  superseded_by?: string
}

export interface Scored {
  memory: Memory
  score: number
}

export interface SearchResponse {
  results: Scored[]
}

export interface ListResponse {
  memories: Memory[]
}

export interface Stats {
  namespace: string
  total: number
  by_tier: Partial<Record<Tier, number>>
  expired: number
  superseded: number
  total_accesses: number
  avg_importance: number
  last_write_at?: string
}

export interface NamespacesResponse {
  namespaces: string[]
}

// Fsck report — mirrors internal/maintenance.Report.
export interface FsckReport {
  expired_purged: number
  short_term_evicted: number
  namespaces: number
  duplicate_groups?: string[][]
}
