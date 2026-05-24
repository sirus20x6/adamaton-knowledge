module github.com/sirus20x6/adamaton-knowledge/r2g

go 1.25.0

require (
	github.com/JohannesKaufmann/html-to-markdown/v2 v2.5.1
	github.com/PuerkitoBio/goquery v1.12.0
	github.com/gen2brain/go-fitz v1.24.15
	github.com/google/uuid v1.6.0
	github.com/gorilla/mux v1.8.1
	github.com/jackc/pgx/v5 v5.9.2
	github.com/pkoukk/tiktoken-go v0.1.8
	github.com/pkoukk/tiktoken-go-loader v0.0.2
	github.com/prometheus/client_golang v1.23.2
	github.com/sirupsen/logrus v1.9.4
	github.com/sirus20x6/adamaton-core v0.0.0
	github.com/sirus20x6/ztok-go v0.0.0
	golang.org/x/image v0.40.0
)

require (
	github.com/JohannesKaufmann/dom v0.2.0 // indirect
	github.com/andybalholm/cascadia v1.3.3 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dlclark/regexp2 v1.10.0 // indirect
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jupiterrider/ffi v0.5.0 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace (
	github.com/sirus20x6/adamaton-core => ../../core
	github.com/sirus20x6/ztok-go => ../../ztok/bindings/go
	github.com/sirus20x6/adamaton-deepresearch/nano-research => ../../deepresearch/nano-research
	github.com/sirus20x6/adamaton-delegator/delegator => ../../delegator/delegator
	github.com/sirus20x6/adamaton-delegator/mcp => ../../delegator/mcp
	github.com/sirus20x6/adamaton-evolve/evolve => ../../evolve/evolve
	github.com/sirus20x6/adamaton-evolve/workflow-builder => ../../evolve/workflow-builder
	github.com/sirus20x6/adamaton-knowledge/r2g => ../../knowledge/r2g
	github.com/sirus20x6/adamaton-knowledge/reindex => ../../knowledge/reindex
	github.com/sirus20x6/adamaton-knowledge/skills => ../../knowledge/skills
	github.com/sirus20x6/adamaton-knowledge/skills-rae => ../../knowledge/skills-rae
	github.com/sirus20x6/adamaton-platform/dashboard => ../../platform/dashboard
	github.com/sirus20x6/adamaton-platform/dispatch => ../../platform/dispatch
	github.com/sirus20x6/adamaton-platform/plugin-host => ../../platform/plugin-host
	github.com/sirus20x6/adamaton-platform/temporal => ../../platform/temporal
)
