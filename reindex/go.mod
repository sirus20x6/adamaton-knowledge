module github.com/sirus20x6/adamaton-knowledge/reindex

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/sirupsen/logrus v1.9.4
	github.com/sirus20x6/adamaton-core v0.0.0
	go.temporal.io/sdk v1.43.0
)


require go.temporal.io/api v1.62.11

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/facebookgo/clock v0.0.0-20150410010913-600d898af40a // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/mock v1.6.0 // indirect
	github.com/grpc-ecosystem/go-grpc-middleware/v2 v2.3.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.22.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/nexus-rpc/sdk-go v0.6.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/robfig/cron v1.2.0 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/time v0.12.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260120221211-b8f7ae30c516 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260120221211-b8f7ae30c516 // indirect
	google.golang.org/grpc v1.79.3 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/sirus20x6/adamaton-core => ../../core
	github.com/sirus20x6/adamaton-knowledge/skills => ../../knowledge/skills
	github.com/sirus20x6/adamaton-knowledge/skills-rae => ../../knowledge/skills-rae
	github.com/sirus20x6/adamaton-knowledge/reindex => ../../knowledge/reindex
	github.com/sirus20x6/adamaton-knowledge/r2g => ../../knowledge/r2g
	github.com/sirus20x6/adamaton-deepresearch/nano-research => ../../deepresearch/nano-research
	github.com/sirus20x6/adamaton-delegator/delegator => ../../delegator/delegator
	github.com/sirus20x6/adamaton-delegator/mcp => ../../delegator/mcp
	github.com/sirus20x6/adamaton-evolve/evolve => ../../evolve/evolve
	github.com/sirus20x6/adamaton-evolve/workflow-builder => ../../evolve/workflow-builder
	github.com/sirus20x6/adamaton-platform/dashboard => ../../platform/dashboard
	github.com/sirus20x6/adamaton-platform/plugin-host => ../../platform/plugin-host
	github.com/sirus20x6/adamaton-platform/dispatch => ../../platform/dispatch
	github.com/sirus20x6/adamaton-platform/temporal => ../../platform/temporal
)
