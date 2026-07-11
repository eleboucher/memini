// API types derived from api/openapi.yaml via `npm run gen-api`
// (openapi-typescript -> src/api-schema.gen.ts). Don't hand-edit shapes here;
// change the spec and regenerate, so the UI can't drift from the server.

import type { components } from './api-schema.gen'

type Schemas = components['schemas']

export type Tier = Schemas['Tier']

export const TIERS: Tier[] = ['working', 'episodic', 'semantic', 'procedural']

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
