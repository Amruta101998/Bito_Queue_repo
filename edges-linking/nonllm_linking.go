package edgeslinking

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"gitlab.com/bitoco/cis-index/edges-linking/index"
	"gitlab.com/bitoco/cis-index/edges-linking/matcher"
	"gitlab.com/bitoco/cis-index/edges-linking/models"
	"gitlab.com/bitoco/cis-index/edges-linking/storage"
)

// NonLLMLinkingConfig holds configuration for non-LLM linking.
type NonLLMLinkingConfig struct {
	// Matching configuration
	MatchConfig *matcher.MatchConfig `json:"match_config"`

	// Logging configuration
	Logging models.LoggingConfig `json:"logging"`
}

// DefaultNonLLMLinkingConfig returns the default configuration for non-LLM linking.
func DefaultNonLLMLinkingConfig() *NonLLMLinkingConfig {
	return &NonLLMLinkingConfig{
		MatchConfig: matcher.DefaultMatchConfig(),
		Logging: models.LoggingConfig{
			Level:   "info",
			Console: true,
		},
	}
}

// RunNonLLMLinking performs non-LLM based edges linking for all repositories.
// This is the main entry point for full indexing (from scratch).
func RunNonLLMLinking(ctx context.Context, input *models.LinkingInput) (*models.LinkingOutput, error) {
	startTime := time.Now()

	// Load configuration
	config := DefaultNonLLMLinkingConfig()
	if len(input.ModuleConfig) > 0 {
		if err := json.Unmarshal(input.ModuleConfig, config); err != nil {
			// If config parsing fails, use defaults
			config = DefaultNonLLMLinkingConfig()
		}
	}

	logger := initializeNonLLMLogger(config.Logging)
	logger.WithFields(logrus.Fields{
		"total_repositories": len(input.Repositories),
		"method":             "non-llm",
	}).Info("[non-llm-linking] Starting edges linking process")

	// Initialize storage client
	storageClient, err := storage.NewStorageClient(input.AggregateStorageConfig.Type)
	if err != nil {
		logger.WithError(err).Error("[non-llm-linking] Failed to initialize storage client")
		return &models.LinkingOutput{
			Status:     models.LinkingStatusFailure,
			UpdateType: models.UpdateTypeScratch,
			Error:      fmt.Sprintf("failed to initialize storage client: %v", err),
		}, err
	}

	// Phase 1: Load all repository edges
	logger.Info("[non-llm-linking] Phase 1: Loading repository edges")
	repoEdges, err := loadAllRepositoryEdges(input.Repositories, storageClient, logger)
	if err != nil {
		logger.WithError(err).Error("[non-llm-linking] Failed to load repository edges")
		return &models.LinkingOutput{
			Status:     models.LinkingStatusFailure,
			UpdateType: models.UpdateTypeScratch,
			Error:      fmt.Sprintf("failed to load repository edges: %v", err),
		}, err
	}

	// Phase 2: Build inverted index
	logger.Info("[non-llm-linking] Phase 2: Building inverted index")
	indexStartTime := time.Now()
	edgeIndex := index.BuildIndex(repoEdges, logger)
	indexDuration := time.Since(indexStartTime)

	indexStats := edgeIndex.GetStats()
	logger.WithFields(logrus.Fields{
		"repos_indexed":    indexStats.ReposIndexed,
		"total_providers":  indexStats.TotalProviders,
		"unique_endpoints": indexStats.UniqueEndpoints,
		"total_table_refs": indexStats.TotalTableRefs,
		"unique_tables":    indexStats.UniqueTables,
		"total_queue_refs": indexStats.TotalQueueRefs,
		"unique_topics":    indexStats.UniqueTopics,
		"duration_ms":      indexDuration.Milliseconds(),
	}).Info("[non-llm-linking] Index built successfully")

	// Phase 3: Find all links
	logger.Info("[non-llm-linking] Phase 3: Finding cross-repository links")
	matchStartTime := time.Now()
	linkMatcher := matcher.NewMatcher(config.MatchConfig, logger)
	links := linkMatcher.FindAllLinks(repoEdges, edgeIndex)
	matchDuration := time.Since(matchStartTime)

	linkStats := matcher.GetLinkStats(links)
	logger.WithFields(logrus.Fields{
		"total_links":    linkStats.TotalLinks,
		"api_links":      linkStats.APILinks,
		"database_links": linkStats.DatabaseLinks,
		"queue_links":    linkStats.QueueLinks,
		"exact_matches":  linkStats.ExactMatches,
		"fuzzy_matches":  linkStats.FuzzyMatches,
		"avg_confidence": fmt.Sprintf("%.2f", linkStats.AvgConfidence),
		"duration_ms":    matchDuration.Milliseconds(),
	}).Info("[non-llm-linking] Links discovered")

	// Phase 4: Transform results and write output
	logger.Info("[non-llm-linking] Phase 4: Writing output")
	allRepoLinks := make([]*models.RepositoryLinks, len(repoEdges))

	for i, repoInput := range input.Repositories {
		repoName := storage.GetRepositoryName(repoInput.URL)
		repoLinks := matcher.TransformToRepositoryLinks(repoEdges[i], links, logger)
		allRepoLinks[i] = repoLinks

		if err := storageClient.WriteAggregateResult(input.AggregateStorageConfig, repoName, repoLinks); err != nil {
			logger.WithFields(logrus.Fields{
				"repo":  repoName,
				"error": err,
			}).Error("[non-llm-linking] Failed to write aggregate result")
			return &models.LinkingOutput{
				Status:     models.LinkingStatusFailure,
				UpdateType: models.UpdateTypeScratch,
				Error:      fmt.Sprintf("failed to write result for %s: %v", repoName, err),
			}, err
		}

		logger.WithFields(logrus.Fields{
			"repo":      repoName,
			"calls":     len(repoLinks.Calls),
			"providers": len(repoLinks.Providers),
		}).Debug("[non-llm-linking] Written repository links")
	}

	// Phase 5: Populate reverse links (called_by on providers)
	logger.Info("[non-llm-linking] Phase 5: Populating reverse links (bidirectional linking)")
	matcher.PopulateReverseLinks(allRepoLinks, logger)

	// Write updated results with reverse links
	for i, repoInput := range input.Repositories {
		repoName := storage.GetRepositoryName(repoInput.URL)
		if err := storageClient.WriteAggregateResult(input.AggregateStorageConfig, repoName, allRepoLinks[i]); err != nil {
			logger.WithFields(logrus.Fields{
				"repo":  repoName,
				"error": err,
			}).Error("[non-llm-linking] Failed to write updated aggregate result")
			return &models.LinkingOutput{
				Status:     models.LinkingStatusFailure,
				UpdateType: models.UpdateTypeScratch,
				Error:      fmt.Sprintf("failed to write updated result for %s: %v", repoName, err),
			}, err
		}
	}

	totalDuration := time.Since(startTime)
	logger.WithFields(logrus.Fields{
		"total_duration_ms": totalDuration.Milliseconds(),
		"repos_processed":   len(input.Repositories),
		"links_discovered":  linkStats.TotalLinks,
		"llm_calls":         0,
	}).Info("[non-llm-linking] Edges linking process completed successfully")

	return &models.LinkingOutput{
		Status:               models.LinkingStatusSuccess,
		UpdateType:           models.UpdateTypeScratch,
		FailureRecoveryState: nil,
	}, nil
}

// RunNonLLMIncrementalLinking performs non-LLM based incremental edges linking.
// This is used when new repositories are added to an existing workspace.
func RunNonLLMIncrementalLinking(ctx context.Context, input *models.IncrementalLinkingInput) (*models.IncrementalLinkingOutput, error) {
	startTime := time.Now()

	// Validate input
	if err := validateNonLLMIncrementalInput(input); err != nil {
		return &models.IncrementalLinkingOutput{
			Status: models.LinkingStatusFailure,
			Error:  fmt.Sprintf("Invalid input: %v", err),
		}, err
	}

	// Load configuration
	config := DefaultNonLLMLinkingConfig()
	if len(input.ModuleConfig) > 0 {
		if err := json.Unmarshal(input.ModuleConfig, config); err != nil {
			config = DefaultNonLLMLinkingConfig()
		}
	}

	logger := initializeNonLLMLogger(config.Logging)
	logger.WithFields(logrus.Fields{
		"linked_repos_path":   input.LinkedReposPath,
		"unlinked_repos_path": input.UnlinkedReposPath,
		"unlinked_count":      len(input.UnlinkedRepositories),
		"method":              "non-llm",
	}).Info("[non-llm-linking] Starting incremental linking")

	// Phase 1: Load existing linked repositories
	logger.Info("[non-llm-linking] Phase 1: Loading existing linked repositories")
	linkedRepos, err := loadLinkedRepositories(input.LinkedReposPath, logger)
	if err != nil {
		logger.WithError(err).Error("[non-llm-linking] Failed to load linked repositories")
		return &models.IncrementalLinkingOutput{
			Status: models.LinkingStatusFailure,
			Error:  fmt.Sprintf("failed to load linked repositories: %v", err),
		}, err
	}
	logger.WithFields(logrus.Fields{
		"count": len(linkedRepos),
	}).Info("[non-llm-linking] Loaded linked repositories")

	// Phase 2: Load new unlinked repositories
	logger.Info("[non-llm-linking] Phase 2: Loading unlinked repositories")
	unlinkedRepos, err := loadUnlinkedRepositories(input.UnlinkedReposPath, input.UnlinkedRepositories, logger)
	if err != nil {
		logger.WithError(err).Error("[non-llm-linking] Failed to load unlinked repositories")
		return &models.IncrementalLinkingOutput{
			Status: models.LinkingStatusFailure,
			Error:  fmt.Sprintf("failed to load unlinked repositories: %v", err),
		}, err
	}
	logger.WithFields(logrus.Fields{
		"count": len(unlinkedRepos),
	}).Info("[non-llm-linking] Loaded unlinked repositories")

	// Phase 3: Build index from existing linked repos
	logger.Info("[non-llm-linking] Phase 3: Building index from existing linked repos")
	linkedIndex := index.BuildIndexFromLinkedRepos(linkedRepos, logger)

	// Phase 4: Build index from new unlinked repos
	logger.Info("[non-llm-linking] Phase 4: Building index from new unlinked repos")
	unlinkedEdges := make([]*models.RepositoryEdges, 0, len(unlinkedRepos))
	for _, edges := range unlinkedRepos {
		unlinkedEdges = append(unlinkedEdges, edges)
	}
	unlinkedIndex := index.BuildIndex(unlinkedEdges, logger)

	// Merge indices for complete lookup
	combinedIndex := index.NewEdgeIndex(logger)
	combinedIndex.MergeIndex(linkedIndex)
	combinedIndex.MergeIndex(unlinkedIndex)

	logger.WithFields(logrus.Fields{
		"total_providers":  combinedIndex.GetStats().TotalProviders,
		"unique_endpoints": combinedIndex.GetStats().UniqueEndpoints,
	}).Info("[non-llm-linking] Combined index built")

	// Phase 5: Find links for unlinked repos against combined index
	logger.Info("[non-llm-linking] Phase 5: Finding links for new repositories")
	linkMatcher := matcher.NewMatcher(config.MatchConfig, logger)
	newLinks := linkMatcher.FindAllLinks(unlinkedEdges, combinedIndex)

	// Also find links from existing linked repos to new repos (using unlinked index)
	// We need to treat calls from linked repos as well
	linkedEdges := convertLinkedReposToEdges(linkedRepos)
	reverseLinks := linkMatcher.FindAllLinks(linkedEdges, unlinkedIndex)

	// Combine all links
	allLinks := append(newLinks, reverseLinks...)
	allLinks = deduplicateLinks(allLinks)

	linkStats := matcher.GetLinkStats(allLinks)
	logger.WithFields(logrus.Fields{
		"total_links":    linkStats.TotalLinks,
		"api_links":      linkStats.APILinks,
		"database_links": linkStats.DatabaseLinks,
		"queue_links":    linkStats.QueueLinks,
	}).Info("[non-llm-linking] Links discovered for incremental update")

	// Phase 6: Create/update repository links files
	logger.Info("[non-llm-linking] Phase 6: Writing output")

	// Ensure output directory exists
	if err := os.MkdirAll(input.OutputPath, 0755); err != nil {
		return &models.IncrementalLinkingOutput{
			Status: models.LinkingStatusFailure,
			Error:  fmt.Sprintf("failed to create output directory: %v", err),
		}, err
	}

	// Track write failures for reporting
	// Note: Current model doesn't support partial success reporting in output structure,
	// so we log failures but continue processing to preserve partial success
	writeFailures := make(map[string]error)

	// Transform and write unlinked repos (new repos)
	allRepoLinks := make([]*models.RepositoryLinks, 0)
	for repoName, edges := range unlinkedRepos {
		repoLinks := matcher.TransformToRepositoryLinks(edges, allLinks, logger)
		allRepoLinks = append(allRepoLinks, repoLinks)

		if err := writeRepositoryLinksToFile(input.OutputPath, repoName, repoLinks); err != nil {
			writeFailures[repoName] = err
			logger.WithFields(logrus.Fields{
				"repo":  repoName,
				"error": err,
			}).Error("[non-llm-linking] Failed to write repository links")
		}
	}

	// Update existing linked repos with new links
	for repoName, existingLinks := range linkedRepos {
		// Copy existing file to output if not already there
		outputPath := filepath.Join(input.OutputPath, repoName+".json")
		if _, err := os.Stat(outputPath); os.IsNotExist(err) {
			// Copy from linked repos path
			srcPath := filepath.Join(input.LinkedReposPath, repoName+".json")
			if data, err := os.ReadFile(srcPath); err == nil {
				if err := os.WriteFile(outputPath, data, 0644); err != nil {
					logger.WithFields(logrus.Fields{
						"repo":  repoName,
						"error": err,
					}).Warn("[non-llm-linking] Failed to copy linked repo file")
				}
			}
		}

		// Add new links to existing repo
		updatedLinks := addLinksToExistingRepo(existingLinks, allLinks, repoName)
		allRepoLinks = append(allRepoLinks, updatedLinks)

		if err := writeRepositoryLinksToFile(input.OutputPath, repoName, updatedLinks); err != nil {
			writeFailures[repoName] = err
			logger.WithFields(logrus.Fields{
				"repo":  repoName,
				"error": err,
			}).Error("[non-llm-linking] Failed to write updated repository links")
		}
	}

	// Phase 7: Populate reverse links
	logger.Info("[non-llm-linking] Phase 7: Populating reverse links (bidirectional linking)")
	matcher.PopulateReverseLinks(allRepoLinks, logger)

	// Write final results
	for _, repoLinks := range allRepoLinks {
		if err := writeRepositoryLinksToFile(input.OutputPath, repoLinks.RepositoryName, repoLinks); err != nil {
			writeFailures[repoLinks.RepositoryName] = err
			logger.WithFields(logrus.Fields{
				"repo":  repoLinks.RepositoryName,
				"error": err,
			}).Error("[non-llm-linking] Failed to write final repository links")
		}
	}

	totalDuration := time.Since(startTime)

	// Report write failures summary if any occurred
	if len(writeFailures) > 0 {
		failedRepos := make([]string, 0, len(writeFailures))
		for repo := range writeFailures {
			failedRepos = append(failedRepos, repo)
		}
		logger.WithFields(logrus.Fields{
			"failed_repos_count": len(writeFailures),
			"failed_repos":       strings.Join(failedRepos, ", "),
			"total_repos":        len(input.UnlinkedRepositories) + len(linkedRepos),
		}).Warn("[non-llm-linking] Some repositories failed to write, partial success achieved")

		logger.WithFields(logrus.Fields{
			"total_duration_ms": totalDuration.Milliseconds(),
			"new_repos":         len(input.UnlinkedRepositories),
			"existing_repos":    len(linkedRepos),
			"links_discovered":  linkStats.TotalLinks,
			"successful_writes": len(input.UnlinkedRepositories) + len(linkedRepos) - len(writeFailures),
			"failed_writes":     len(writeFailures),
			"llm_calls":         0,
		}).Info("[non-llm-linking] Incremental linking completed with partial success")
	} else {
		logger.WithFields(logrus.Fields{
			"total_duration_ms": totalDuration.Milliseconds(),
			"new_repos":         len(input.UnlinkedRepositories),
			"existing_repos":    len(linkedRepos),
			"links_discovered":  linkStats.TotalLinks,
			"llm_calls":         0,
		}).Info("[non-llm-linking] Incremental linking completed successfully")
	}

	return &models.IncrementalLinkingOutput{
		Status: models.LinkingStatusSuccess,
	}, nil
}

// RunNonLLMLinkingFromJSON performs non-LLM based edges linking from JSON input.
func RunNonLLMLinkingFromJSON(ctx context.Context, inputJSON string) (*models.LinkingOutput, error) {
	var input models.LinkingInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return &models.LinkingOutput{
			Status:     models.LinkingStatusFailure,
			UpdateType: models.UpdateTypeScratch,
			Error:      fmt.Sprintf("failed to parse input JSON: %v", err),
		}, err
	}
	return RunNonLLMLinking(ctx, &input)
}

// RunNonLLMLinkingFromFile performs non-LLM based edges linking from file input.
func RunNonLLMLinkingFromFile(ctx context.Context, inputFilePath string) (*models.LinkingOutput, error) {
	data, err := os.ReadFile(inputFilePath)
	if err != nil {
		return &models.LinkingOutput{
			Status:     models.LinkingStatusFailure,
			UpdateType: models.UpdateTypeScratch,
			Error:      fmt.Sprintf("failed to read input file: %v", err),
		}, err
	}
	return RunNonLLMLinkingFromJSON(ctx, string(data))
}

// Helper functions

func initializeNonLLMLogger(logConfig models.LoggingConfig) *logrus.Logger {
	logger := logrus.New()
	level, err := logrus.ParseLevel(logConfig.Level)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})

	if logConfig.File != "" {
		file, err := os.OpenFile(logConfig.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err == nil {
			if logConfig.Console {
				logger.SetOutput(file)
				logger.AddHook(&consoleHook{})
			} else {
				logger.SetOutput(file)
			}
		} else {
			logger.Warnf("Failed to open log file %s, using console only: %v", logConfig.File, err)
			logger.SetOutput(os.Stdout)
		}
	} else {
		logger.SetOutput(os.Stdout)
	}

	return logger
}

func loadAllRepositoryEdges(repositories []models.RepositoryInput, storageClient storage.StorageClient, logger *logrus.Logger) ([]*models.RepositoryEdges, error) {
	repoEdges := make([]*models.RepositoryEdges, len(repositories))

	for i, repo := range repositories {
		repoName := storage.GetRepositoryName(repo.URL)
		logger.WithFields(logrus.Fields{
			"repo": repoName,
			"path": repo.StorageConfig.OutputPath,
		}).Debug("[non-llm-linking] Loading repository edges")

		edges, err := storageClient.ReadRepositoryEdges(repo.StorageConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to read edges for %s: %w", repoName, err)
		}
		edges.Name = repoName
		repoEdges[i] = edges

		logger.WithFields(logrus.Fields{
			"repo":           repoName,
			"api_calls":      len(edges.APICalls),
			"api_providers":  len(edges.APIProviders),
			"database_calls": len(edges.DatabaseCalls),
			"queue_calls":    len(edges.QueueCalls),
		}).Debug("[non-llm-linking] Loaded repository edges")
	}

	return repoEdges, nil
}

func validateNonLLMIncrementalInput(input *models.IncrementalLinkingInput) error {
	if input.LinkedReposPath == "" {
		return fmt.Errorf("linkedReposPath is required")
	}
	if input.UnlinkedReposPath == "" {
		return fmt.Errorf("unlinkedReposPath is required")
	}
	if input.OutputPath == "" {
		return fmt.Errorf("outputPath is required")
	}
	if len(input.UnlinkedRepositories) == 0 {
		return fmt.Errorf("at least one unlinked repository is required")
	}
	return nil
}

func loadLinkedRepositories(linkedReposPath string, logger *logrus.Logger) (map[string]*models.RepositoryLinks, error) {
	entries, err := os.ReadDir(linkedReposPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read linked repos directory: %w", err)
	}

	linkedRepos := make(map[string]*models.RepositoryLinks)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		repoName := strings.TrimSuffix(entry.Name(), ".json")
		filePath := filepath.Join(linkedReposPath, entry.Name())

		data, err := os.ReadFile(filePath)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"file":  filePath,
				"error": err,
			}).Warn("[non-llm-linking] Failed to read linked repo file")
			continue
		}

		var repoLinks models.RepositoryLinks
		if err := json.Unmarshal(data, &repoLinks); err != nil {
			logger.WithFields(logrus.Fields{
				"file":  filePath,
				"error": err,
			}).Warn("[non-llm-linking] Failed to unmarshal linked repo")
			continue
		}

		linkedRepos[repoName] = &repoLinks
	}

	return linkedRepos, nil
}

func loadUnlinkedRepositories(unlinkedReposPath string, repoNames []string, logger *logrus.Logger) (map[string]*models.RepositoryEdges, error) {
	unlinkedRepos := make(map[string]*models.RepositoryEdges)

	for _, repoName := range repoNames {
		repoDir := filepath.Join(unlinkedReposPath, repoName)

		edges := &models.RepositoryEdges{
			Name:          repoName,
			APICallChains: []models.APICallChain{},
		}

		// Load API calls
		apiCallsPath := filepath.Join(repoDir, "api_calls.json")
		if data, err := os.ReadFile(apiCallsPath); err == nil {
			var apiCallsOutput struct {
				APICalls []models.APICall `json:"api_calls"`
			}
			if err := json.Unmarshal(data, &apiCallsOutput); err == nil {
				edges.APICalls = apiCallsOutput.APICalls
			}
		}

		// Load API providers
		apiProvidersPath := filepath.Join(repoDir, "api_providers.json")
		if data, err := os.ReadFile(apiProvidersPath); err == nil {
			var apiProvidersOutput struct {
				APIProviders  []models.APIProvider  `json:"api_definitions"`
				APICallChains []models.APICallChain `json:"api_call_chains"`
			}
			if err := json.Unmarshal(data, &apiProvidersOutput); err == nil {
				edges.APIProviders = apiProvidersOutput.APIProviders
				edges.APICallChains = apiProvidersOutput.APICallChains
			}
		}

		// Load database calls
		databaseCallsPath := filepath.Join(repoDir, "database_calls.json")
		if data, err := os.ReadFile(databaseCallsPath); err == nil {
			var databaseCallsOutput struct {
				DatabaseCalls []models.DatabaseCall `json:"database_calls"`
			}
			if err := json.Unmarshal(data, &databaseCallsOutput); err == nil {
				edges.DatabaseCalls = databaseCallsOutput.DatabaseCalls
			}
		}

		// Load queue calls
		queueCallsPath := filepath.Join(repoDir, "queue_calls.json")
		if data, err := os.ReadFile(queueCallsPath); err == nil {
			var queueCallsOutput struct {
				QueueCalls []models.QueueCall `json:"queue_calls"`
			}
			if err := json.Unmarshal(data, &queueCallsOutput); err == nil {
				edges.QueueCalls = queueCallsOutput.QueueCalls
			}
		}

		unlinkedRepos[repoName] = edges
		logger.WithFields(logrus.Fields{
			"repo":           repoName,
			"api_calls":      len(edges.APICalls),
			"api_providers":  len(edges.APIProviders),
			"database_calls": len(edges.DatabaseCalls),
			"queue_calls":    len(edges.QueueCalls),
		}).Debug("[non-llm-linking] Loaded unlinked repository")
	}

	return unlinkedRepos, nil
}

func writeRepositoryLinksToFile(outputPath, repoName string, links *models.RepositoryLinks) error {
	filePath := filepath.Join(outputPath, repoName+".json")
	data, err := json.MarshalIndent(links, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal repository links: %w", err)
	}
	return os.WriteFile(filePath, data, 0644)
}

func convertLinkedReposToEdges(linkedRepos map[string]*models.RepositoryLinks) []*models.RepositoryEdges {
	edges := make([]*models.RepositoryEdges, 0, len(linkedRepos))

	for repoName, repoLinks := range linkedRepos {
		edge := &models.RepositoryEdges{
			Name:          repoName,
			APICalls:      make([]models.APICall, 0),
			APIProviders:  make([]models.APIProvider, 0),
			DatabaseCalls: make([]models.DatabaseCall, 0),
			QueueCalls:    make([]models.QueueCall, 0),
			APICallChains: repoLinks.APICallChains,
		}

		// Convert calls
		for _, call := range repoLinks.Calls {
			switch call.Type {
			case "api_call":
				edge.APICalls = append(edge.APICalls, models.APICall{
					Repo:        call.Repository,
					File:        call.File,
					Line:        call.Line,
					Subtype:     call.Subtype,
					Identifier:  call.Identifier,
					Description: call.Description,
				})
			case "database_call":
				edge.DatabaseCalls = append(edge.DatabaseCalls, models.DatabaseCall{
					Repo:        call.Repository,
					File:        call.File,
					Line:        call.Line,
					Subtype:     call.Subtype,
					Identifier:  call.Identifier,
					Description: call.Description,
				})
			case "queue_call":
				edge.QueueCalls = append(edge.QueueCalls, models.QueueCall{
					Repo:        call.Repository,
					File:        call.File,
					Line:        call.Line,
					Subtype:     call.Subtype,
					Identifier:  call.Identifier,
					Description: call.Description,
				})
			}
		}

		// Convert providers
		for _, provider := range repoLinks.Providers {
			edge.APIProviders = append(edge.APIProviders, models.APIProvider{
				Repo:           provider.Repository,
				File:           provider.File,
				Line:           provider.Line,
				Identifier:     provider.Identifier,
				Description:    provider.Description,
				RequestSchema:  provider.RequestSchema,
				ResponseSchema: provider.ResponseSchema,
			})
		}

		edges = append(edges, edge)
	}

	return edges
}

func addLinksToExistingRepo(existingLinks *models.RepositoryLinks, allLinks []matcher.Link, repoName string) *models.RepositoryLinks {
	// Create a deep copy of existing links
	result := &models.RepositoryLinks{
		RepositoryName: existingLinks.RepositoryName,
		Calls:          make([]models.CallWithLink, len(existingLinks.Calls)),
		Providers:      make([]models.ProviderWithCallers, len(existingLinks.Providers)),
		APICallChains:  existingLinks.APICallChains,
	}

	// Deep copy existing calls (including LinkedTo slices)
	for i, call := range existingLinks.Calls {
		result.Calls[i] = models.CallWithLink{
			Repository:  call.Repository,
			File:        call.File,
			Line:        call.Line,
			Type:        call.Type,
			Subtype:     call.Subtype,
			Identifier:  call.Identifier,
			Description: call.Description,
			LinkedTo:    make([]models.LinkedItem, len(call.LinkedTo)),
		}
		// Deep copy LinkedTo slice
		copy(result.Calls[i].LinkedTo, call.LinkedTo)
	}

	// Deep copy existing providers (including CalledBy slices)
	for i, provider := range existingLinks.Providers {
		result.Providers[i] = models.ProviderWithCallers{
			Repository:     provider.Repository,
			File:           provider.File,
			Line:           provider.Line,
			Type:           provider.Type,
			Identifier:     provider.Identifier,
			Description:    provider.Description,
			RequestSchema:  provider.RequestSchema,
			ResponseSchema: provider.ResponseSchema,
			CalledBy:       make([]models.CallerItem, len(provider.CalledBy)),
		}
		// Deep copy CalledBy slice
		copy(result.Providers[i].CalledBy, provider.CalledBy)
	}

	// Build call map for quick lookup
	callMap := make(map[string]*models.CallWithLink)
	for i := range result.Calls {
		key := fmt.Sprintf("%s|%s", result.Calls[i].Type, result.Calls[i].Identifier)
		callMap[key] = &result.Calls[i]
	}

	// Add new links from this repo to others
	for _, link := range allLinks {
		if link.FromRepo != repoName {
			continue
		}

		callType := link.LinkType + "_call"
		if link.LinkType == "api" {
			callType = "api_call"
		}
		key := fmt.Sprintf("%s|%s", callType, link.CallerIdentifier)

		if call, exists := callMap[key]; exists {
			// Check if link already exists (match on Repository, File, Line, and Identifier)
			exists := false
			for _, existing := range call.LinkedTo {
				if existing.Repository == link.ToRepo &&
					existing.File == link.ProviderFile &&
					existing.Line == link.ProviderLine &&
					existing.Identifier == link.ProviderIdentifier {
					exists = true
					break
				}
			}
			if !exists {
				call.LinkedTo = append(call.LinkedTo, models.LinkedItem{
					Repository:  link.ToRepo,
					File:        link.ProviderFile,
					Line:        link.ProviderLine,
					Identifier:  link.ProviderIdentifier,
					Confidence:  fmt.Sprintf("%.2f", link.Confidence),
					Description: "",
				})
			}
		}
	}

	return result
}

func deduplicateLinks(links []matcher.Link) []matcher.Link {
	seen := make(map[string]bool)
	var unique []matcher.Link

	for _, link := range links {
		key := fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s|%d|%s",
			link.LinkType,
			link.FromRepo,
			link.ToRepo,
			link.CallerFile,
			link.CallerLine,
			link.CallerIdentifier,
			link.ProviderFile,
			link.ProviderLine,
			link.ProviderIdentifier,
		)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, link)
		}
	}

	return unique
}
