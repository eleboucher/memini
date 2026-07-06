module github.com/eleboucher/memini

go 1.26.4

require (
	github.com/asg017/sqlite-vec-go-bindings v0.1.6
	github.com/go-chi/chi/v5 v5.3.0
	// Pinned: newest ncruces with both sqlite3.Binary and wasm atomics, required
	// by the sqlite-vec-go-bindings embedded wasm. See .renovaterc.json5.
	github.com/ncruces/go-sqlite3 v0.21.2
	github.com/prometheus/client_golang v1.23.2
)

require (
	github.com/anthropics/anthropic-sdk-go v1.56.0
	github.com/caarlos0/env/v11 v11.4.1
	github.com/google/uuid v1.6.0
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/jackc/pgx/v5 v5.10.0
	github.com/modelcontextprotocol/go-sdk v1.6.1
	github.com/oapi-codegen/runtime v1.4.2
	github.com/openai/openai-go/v3 v3.41.0
	github.com/pgvector/pgvector-go v0.4.0
	github.com/pgvector/pgvector-go/pgx v0.4.0
	github.com/prometheus/client_model v0.6.2
	github.com/spf13/cobra v1.10.2
	golang.org/x/sync v0.21.0
	golang.org/x/term v0.44.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dprotaso/go-yit v0.0.0-20220510233725-9ba8df137936 // indirect
	github.com/getkin/kin-openapi v0.135.0 // indirect
	github.com/go-openapi/jsonpointer v0.22.4 // indirect
	github.com/go-openapi/swag/jsonname v0.25.4 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/mailru/easyjson v0.9.1 // indirect
	github.com/mohae/deepcopy v0.0.0-20170929034955-c48cc78d4826 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ncruces/julianday v1.0.0 // indirect
	github.com/oapi-codegen/oapi-codegen/v2 v2.7.1 // indirect
	github.com/oasdiff/yaml v0.0.9 // indirect
	github.com/oasdiff/yaml3 v0.0.9 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/perimeterx/marshmallow v1.1.5 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/speakeasy-api/jsonpath v0.6.3 // indirect
	github.com/speakeasy-api/openapi v1.19.2 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tetratelabs/wazero v1.9.0 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/vmware-labs/yaml-jsonpath v0.3.2 // indirect
	github.com/woodsbury/decimal128 v1.4.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/mod v0.34.0 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	golang.org/x/tools v0.43.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

tool github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
