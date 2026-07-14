// API types derived from api/openapi.yaml via `npm run gen-api`
// (openapi-typescript -> src/api-schema.gen.ts). Don't hand-edit shapes here;
// change the spec and regenerate, so the UI can't drift from the server.

import type { components, operations } from './api-schema.gen'

type Schemas = components['schemas']
type Operations = operations

export type Tier = Schemas['Tier']

export const TIERS: Tier[] = ['working', 'episodic', 'semantic', 'procedural']

export type Level = Schemas['Level']

export const LEVELS: Level[] = ['explicit', 'deduced']

// The memory types the service stamps onto metadata.memory_type
// (service.promote/classify). Not a spec enum — it's a metadata value — so this
// list is the UI's own, matching MemoryTypeBadge's.
export const MEMORY_TYPES = ['decision', 'preference', 'problem'] as const
export type MemoryType = (typeof MEMORY_TYPES)[number]

// Sort keys accepted by GET /v1/memories?sort=, from the generated operation.
export type SortKey = NonNullable<
  NonNullable<Operations['listMemories']['parameters']['query']>['sort']
>
export type SortOrder = NonNullable<
  NonNullable<Operations['listMemories']['parameters']['query']>['order']
>

export const SORT_KEYS: { key: SortKey; label: string }[] = [
  { key: 'created_at', label: 'Created' },
  { key: 'updated_at', label: 'Updated' },
  { key: 'last_accessed_at', label: 'Last used' },
  { key: 'access_count', label: 'Recall count' },
  { key: 'importance', label: 'Importance' },
]

export type EventKind = Schemas['EventKind']
export type ActivityEvent = Schemas['ActivityEvent']
export type ActivityMemory = Schemas['ActivityMemory']
export type ActivityResponse = Schemas['ActivityResponse']

export const EVENT_KINDS: EventKind[] = [
  'recall',
  'briefing',
  'get',
  'remember',
  'update',
  'forget',
  'supersede',
  'pin',
  'unpin',
  'settings',
]

export type Memory = Schemas['Memory']
export type Scored = Schemas['ScoredMemory']
export type SearchResponse = Schemas['SearchResponse']
export type ListResponse = Schemas['ListResponse']
export type Stats = Schemas['Stats']
export type NamespacesResponse = Schemas['NamespacesResponse']
export type FsckReport = Schemas['FsckReport']
export type DedupReport = Schemas['DedupReport']
export type DedupRequest = Schemas['DedupRequest']
export type ClusterAction = Schemas['ClusterAction']
export type RenamespaceReport = Schemas['RenamespaceReport']
export type ReadSetOrigin = Schemas['ReadSetOrigin']
export type ReadSetEntryItem = Schemas['ReadSetEntryItem']
export type ReadSetResponse = Schemas['ReadSetResponse']
export type NamespaceLink = Schemas['NamespaceLink']
export type NamespaceLinksResponse = Schemas['NamespaceLinksResponse']
export type BriefingItem = Schemas['BriefingItem']

export type ApiKey = Schemas['ApiKey']
export type ApiKeySource = Schemas['ApiKeySource']
export type ApiKeyWithSecret = Schemas['ApiKeyWithSecret']
export type ApiKeysResponse = Schemas['ApiKeysResponse']
export type CreateApiKeyRequest = Schemas['CreateApiKeyRequest']

// UpdateApiKeyBody mirrors the spec's UpdateApiKeyRequest but loosens `settings` to a
// partial ClientSettings. The generated ClientSettings type marks every
// field required — openapi-typescript treats a schema `default` as making a
// field always-present in a fully-resolved read, which is right for
// SelfResponse/HandshakeResponse/SettingsDefaultsResponse but overly strict
// here: a per-key settings override is explicitly partial on the wire
// (ApiKey.settings' own description: "fields left unset inherit the
// server's global defaults"). Only the fields present in `settings` should
// ever be serialized — see Keys.tsx's per-key editor.
export interface UpdateApiKeyBody {
  home?: string
  default_namespace?: string
  disabled?: boolean
  admin?: boolean
  settings?: Partial<ClientSettings>
}

// ---- Config view (Phase 8): settings layers, pins, handshake ----------

export type ClientSettings = Schemas['ClientSettings']
export type SettingsSource = 'default' | 'global' | 'key'
export type SettingsDefaultsResponse = Schemas['SettingsDefaultsResponse']
export type CallerIdentity = Schemas['CallerIdentity']
export type SelfResponse = Schemas['SelfResponse']
export type HandshakeRequest = Schemas['HandshakeRequest']
export type HandshakeResponse = Schemas['HandshakeResponse']
export type NamespaceSource = HandshakeResponse['namespace_source']
export type ProjectMapEntry = Schemas['ProjectMapEntry']
export type ProjectMapListResponse = Schemas['ProjectMapListResponse']
export type ProjectMapPutRequest = Schemas['ProjectMapPutRequest']
export type ProjectMapDeleteRequest = Schemas['ProjectMapDeleteRequest']
