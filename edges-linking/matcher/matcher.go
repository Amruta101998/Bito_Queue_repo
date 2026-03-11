package matcher

import (
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	"gitlab.com/bitoco/cis-index/edges-linking/index"
	"gitlab.com/bitoco/cis-index/edges-linking/models"
	"gitlab.com/bitoco/cis-index/edges-linking/normalizer"
)

// MatchConfig holds configuration for matching behavior.
type MatchConfig struct {
	// Confidence scoring weights
	NormalizedMatchWeight float64 `json:"normalized_match_weight"`
	MethodMatchWeight     float64 `json:"method_match_weight"`
	PathSimilarityWeight  float64 `json:"path_similarity_weight"`
	SameOrgWeight         float64 `json:"same_org_weight"`

	// Fuzzy matching settings
	EnableFuzzy    bool    `json:"enable_fuzzy"`
	FuzzyThreshold float64 `json:"fuzzy_threshold"`

	// Minimum confidence to report a link
	MinConfidence float64 `json:"min_confidence"`
}

// DefaultMatchConfig returns a default matching configuration.
func DefaultMatchConfig() *MatchConfig {
	return &MatchConfig{
		NormalizedMatchWeight: 0.5,
		MethodMatchWeight:     0.2,
		PathSimilarityWeight:  0.2,
		SameOrgWeight:         0.1,
		EnableFuzzy:           true,
		FuzzyThreshold:        0.85,
		MinConfidence:         0.5,
	}
}

// Link represents a cross-repository link.
type Link struct {
	LinkType           string  `json:"link_type"` // "api", "database", "queue"
	FromRepo           string  `json:"from_repo"`
	ToRepo             string  `json:"to_repo"`
	CallerFile         string  `json:"caller_file"`
	CallerLine         int     `json:"caller_line"`
	CallerIdentifier   string  `json:"caller_identifier"`
	ProviderFile       string  `json:"provider_file"`
	ProviderLine       int     `json:"provider_line"`
	ProviderIdentifier string  `json:"provider_identifier"`
	Confidence         float64 `json:"confidence"`
	MatchType          string  `json:"match_type"` // "exact", "normalized", "fuzzy"
	SharedResource     string  `json:"shared_resource,omitempty"`
}

// LinkStats holds statistics about discovered links.
type LinkStats struct {
	TotalLinks        int     `json:"total_links"`
	APILinks          int     `json:"api_links"`
	DatabaseLinks     int     `json:"database_links"`
	QueueLinks        int     `json:"queue_links"`
	ExactMatches      int     `json:"exact_matches"`
	NormalizedMatches int     `json:"normalized_matches"`
	FuzzyMatches      int     `json:"fuzzy_matches"`
	AvgConfidence     float64 `json:"avg_confidence"`
	ReposWithOutgoing int     `json:"repos_with_outgoing"`
	ReposWithIncoming int     `json:"repos_with_incoming"`
}

// Matcher performs non-LLM matching using an inverted index.
type Matcher struct {
	config *MatchConfig
	logger *logrus.Logger
}

// NewMatcher creates a new Matcher with the given configuration.
func NewMatcher(config *MatchConfig, logger *logrus.Logger) *Matcher {
	if config == nil {
		config = DefaultMatchConfig()
	}
	return &Matcher{
		config: config,
		logger: logger,
	}
}

// CalculateAPIConfidence calculates confidence score for an API call -> provider match.
// Returns (confidence_score, match_type).
func (m *Matcher) CalculateAPIConfidence(callIdentifier string, provider index.ProviderRef) (float64, string) {
	callNormalized := normalizer.NormalizeEndpoint(callIdentifier)
	providerNormalized := provider.Normalized

	// Check for exact normalized match
	if callNormalized == providerNormalized {
		score := m.config.NormalizedMatchWeight

		// Check method match (extract from normalized form)
		callParts := strings.SplitN(callNormalized, ":", 2)
		providerParts := strings.SplitN(providerNormalized, ":", 2)

		if len(callParts) == 2 && len(providerParts) == 2 {
			if callParts[0] == providerParts[0] {
				score += m.config.MethodMatchWeight
			}

			// Path similarity (should be 1.0 for exact match but computed anyway)
			pathSim := normalizer.CalculatePathSimilarity(callParts[1], providerParts[1])
			score += pathSim * m.config.PathSimilarityWeight
		}

		score += m.config.SameOrgWeight
		if score > 1.0 {
			score = 1.0
		}

		return score, "exact"
	}

	// If not exact match, return 0 (fuzzy matching handled separately)
	return 0.0, "none"
}

// FindFuzzyAPIMatches finds API providers using fuzzy matching when exact match fails.
// Returns list of (provider, similarity_score) pairs.
func (m *Matcher) FindFuzzyAPIMatches(callIdentifier string, idx *index.EdgeIndex, excludeRepos map[string]bool) []struct {
	Provider   index.ProviderRef
	Similarity float64
} {
	if !m.config.EnableFuzzy {
		return nil
	}

	callNormalized := normalizer.NormalizeEndpoint(callIdentifier)
	var matches []struct {
		Provider   index.ProviderRef
		Similarity float64
	}

	// Extract method for filtering
	callMethod := ""
	if colonIdx := strings.Index(callNormalized, ":"); colonIdx != -1 {
		callMethod = callNormalized[:colonIdx]
	}

	allEndpoints := idx.GetAllNormalizedEndpoints()
	for _, endpoint := range allEndpoints {
		// Filter by method if available
		if callMethod != "" && strings.Contains(endpoint, ":") {
			if !strings.HasPrefix(endpoint, callMethod+":") {
				continue
			}
		}

		// Calculate similarity
		similarity := normalizer.JaroWinklerSimilarity(callNormalized, endpoint)

		if similarity >= m.config.FuzzyThreshold {
			providers := idx.LookupAPIProvider(endpoint)
			for _, provider := range providers {
				if excludeRepos != nil && excludeRepos[provider.RepoName] {
					continue
				}
				matches = append(matches, struct {
					Provider   index.ProviderRef
					Similarity float64
				}{Provider: provider, Similarity: similarity})
			}
		}
	}

	// Sort by similarity descending
	for i := 0; i < len(matches); i++ {
		for j := i + 1; j < len(matches); j++ {
			if matches[j].Similarity > matches[i].Similarity {
				matches[i], matches[j] = matches[j], matches[i]
			}
		}
	}

	return matches
}

// FindAPILinks finds all API call -> provider links across repositories.
// Complexity: O(m) where m is the total number of API calls.
func (m *Matcher) FindAPILinks(repositories []*models.RepositoryEdges, idx *index.EdgeIndex) []Link {
	var links []Link

	for _, repo := range repositories {
		if repo == nil {
			continue
		}
		repoName := repo.Name

		for _, call := range repo.APICalls {
			// Skip generic/empty identifiers (health, ping, status, etc.)
			if normalizer.IsGenericEndpoint(call.Identifier) {
				continue
			}

			// O(1) lookup in index with fallback to suffix matching
			providers, lookupType := idx.LookupAPIProviderWithFallback(call.Identifier)

			for _, provider := range providers {
				// Skip self-references
				if provider.RepoName == repoName {
					continue
				}

				var confidence float64
				var matchType string

				if lookupType == "exact" {
					confidence, matchType = m.CalculateAPIConfidence(call.Identifier, provider)
				} else if lookupType == "suffix" {
					// Suffix match - slightly lower confidence
					confidence = 0.85
					matchType = "suffix"
				}

				if confidence >= m.config.MinConfidence {
					links = append(links, Link{
						LinkType:           "api",
						FromRepo:           repoName,
						ToRepo:             provider.RepoName,
						CallerFile:         call.File,
						CallerLine:         call.Line,
						CallerIdentifier:   call.Identifier,
						ProviderFile:       provider.File,
						ProviderLine:       provider.Line,
						ProviderIdentifier: provider.Identifier,
						Confidence:         confidence,
						MatchType:          matchType,
					})
				}
			}

			// Try fuzzy matching if no matches found
			if len(providers) == 0 && m.config.EnableFuzzy {
				excludeRepos := map[string]bool{repoName: true}
				fuzzyMatches := m.FindFuzzyAPIMatches(call.Identifier, idx, excludeRepos)

				// Take top 3 fuzzy matches
				maxFuzzy := 3
				if len(fuzzyMatches) < maxFuzzy {
					maxFuzzy = len(fuzzyMatches)
				}

				for i := 0; i < maxFuzzy; i++ {
					match := fuzzyMatches[i]
					confidence := match.Similarity * 0.8 // Penalize fuzzy matches slightly

					if confidence >= m.config.MinConfidence {
						links = append(links, Link{
							LinkType:           "api",
							FromRepo:           repoName,
							ToRepo:             match.Provider.RepoName,
							CallerFile:         call.File,
							CallerLine:         call.Line,
							CallerIdentifier:   call.Identifier,
							ProviderFile:       match.Provider.File,
							ProviderLine:       match.Provider.Line,
							ProviderIdentifier: match.Provider.Identifier,
							Confidence:         confidence,
							MatchType:          "fuzzy",
						})
					}
				}
			}
		}
	}

	return links
}

// FindDatabaseLinks finds all database table sharing links across repositories.
// When multiple repos access the same table, they're linked.
func (m *Matcher) FindDatabaseLinks(repositories []*models.RepositoryEdges, idx *index.EdgeIndex) []Link {
	var links []Link

	for _, repo := range repositories {
		if repo == nil {
			continue
		}
		repoName := repo.Name

		for _, dbCall := range repo.DatabaseCalls {
			// Skip generic/empty table names
			if normalizer.IsGenericTable(dbCall.Identifier) {
				continue
			}

			// O(1) lookup
			tableRefs := idx.LookupDBTable(dbCall.Identifier)

			for _, ref := range tableRefs {
				// Skip self-references
				if ref.RepoName == repoName {
					continue
				}

				// Exact table name match = high confidence
				links = append(links, Link{
					LinkType:           "database",
					FromRepo:           repoName,
					ToRepo:             ref.RepoName,
					CallerFile:         dbCall.File,
					CallerLine:         dbCall.Line,
					CallerIdentifier:   dbCall.Identifier,
					ProviderFile:       ref.File,
					ProviderLine:       ref.Line,
					ProviderIdentifier: ref.Identifier,
					Confidence:         1.0, // Exact table name match
					MatchType:          "exact",
					SharedResource:     ref.Normalized,
				})
			}
		}
	}

	return links
}

// FindQueueLinks finds all queue producer -> consumer links across repositories.
func (m *Matcher) FindQueueLinks(repositories []*models.RepositoryEdges, idx *index.EdgeIndex) []Link {
	var links []Link

	for _, repo := range repositories {
		if repo == nil {
			continue
		}
		repoName := repo.Name

		for _, queueCall := range repo.QueueCalls {
			// Skip generic/empty queue topics
			if normalizer.IsGenericQueueTopic(queueCall.Identifier) {
				continue
			}

			// O(1) lookup
			topicRefs := idx.LookupQueueTopic(queueCall.Identifier)
			normalized := normalizer.NormalizeQueueTopic(queueCall.Identifier)

			// Determine role
			subtype := strings.ToLower(queueCall.Subtype)
			accessPattern := strings.ToLower(queueCall.AccessPattern)
			isProducer := strings.Contains(subtype, "publish") || strings.Contains(subtype, "produce") ||
				strings.Contains(subtype, "send") || strings.Contains(subtype, "emit") ||
				strings.Contains(subtype, "write") || strings.Contains(subtype, "push") ||
				strings.Contains(accessPattern, "publish") || strings.Contains(accessPattern, "produce") ||
				strings.Contains(accessPattern, "send") || strings.Contains(accessPattern, "emit") ||
				strings.Contains(accessPattern, "write") || strings.Contains(accessPattern, "push")
			isConsumer := strings.Contains(subtype, "subscribe") || strings.Contains(subtype, "consume") ||
				strings.Contains(subtype, "receive") || strings.Contains(subtype, "listen") ||
				strings.Contains(subtype, "read") || strings.Contains(subtype, "pull") ||
				strings.Contains(subtype, "poll") ||
				strings.Contains(accessPattern, "subscribe") || strings.Contains(accessPattern, "consume") ||
				strings.Contains(accessPattern, "receive") || strings.Contains(accessPattern, "listen") ||
				strings.Contains(accessPattern, "read") || strings.Contains(accessPattern, "pull") ||
				strings.Contains(accessPattern, "poll")

			if isProducer {
				// Link producer to all consumers
				for _, consumer := range topicRefs.Consumers {
					if consumer.RepoName == repoName {
						continue
					}

					links = append(links, Link{
						LinkType:           "queue",
						FromRepo:           repoName,
						ToRepo:             consumer.RepoName,
						CallerFile:         queueCall.File,
						CallerLine:         queueCall.Line,
						CallerIdentifier:   queueCall.Identifier,
						ProviderFile:       consumer.File,
						ProviderLine:       consumer.Line,
						ProviderIdentifier: consumer.Identifier,
						Confidence:         1.0,
						MatchType:          "exact",
						SharedResource:     normalized,
					})
				}
			} else if isConsumer {
				// Link consumer back to all producers
				for _, producer := range topicRefs.Producers {
					if producer.RepoName == repoName {
						continue
					}

					links = append(links, Link{
						LinkType:           "queue",
						FromRepo:           producer.RepoName,
						ToRepo:             repoName,
						CallerFile:         producer.File,
						CallerLine:         producer.Line,
						CallerIdentifier:   producer.Identifier,
						ProviderFile:       queueCall.File,
						ProviderLine:       queueCall.Line,
						ProviderIdentifier: queueCall.Identifier,
						Confidence:         1.0,
						MatchType:          "exact",
						SharedResource:     normalized,
					})
				}
			}
		}
	}

	return links
}

// FindAllLinks finds all cross-repository links.
// Total complexity: O(m) where m is total edges across all repos.
func (m *Matcher) FindAllLinks(repositories []*models.RepositoryEdges, idx *index.EdgeIndex) []Link {
	apiLinks := m.FindAPILinks(repositories, idx)
	dbLinks := m.FindDatabaseLinks(repositories, idx)
	queueLinks := m.FindQueueLinks(repositories, idx)

	allLinks := make([]Link, 0, len(apiLinks)+len(dbLinks)+len(queueLinks))
	allLinks = append(allLinks, apiLinks...)
	allLinks = append(allLinks, dbLinks...)
	allLinks = append(allLinks, queueLinks...)

	// Deduplicate links
	return deduplicateLinks(allLinks)
}

// deduplicateLinks removes duplicate links.
func deduplicateLinks(links []Link) []Link {
	seen := make(map[string]bool)
	var unique []Link

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

// GetLinkStats returns statistics about discovered links.
func GetLinkStats(links []Link) LinkStats {
	stats := LinkStats{}
	stats.TotalLinks = len(links)

	reposOutgoing := make(map[string]bool)
	reposIncoming := make(map[string]bool)
	totalConfidence := 0.0

	for _, link := range links {
		switch link.LinkType {
		case "api":
			stats.APILinks++
		case "database":
			stats.DatabaseLinks++
		case "queue":
			stats.QueueLinks++
		}

		switch link.MatchType {
		case "exact":
			stats.ExactMatches++
		case "normalized":
			stats.NormalizedMatches++
		case "fuzzy":
			stats.FuzzyMatches++
		}

		reposOutgoing[link.FromRepo] = true
		reposIncoming[link.ToRepo] = true
		totalConfidence += link.Confidence
	}

	stats.ReposWithOutgoing = len(reposOutgoing)
	stats.ReposWithIncoming = len(reposIncoming)

	if len(links) > 0 {
		stats.AvgConfidence = totalConfidence / float64(len(links))
	}

	return stats
}

// TransformToRepositoryLinks transforms links into the RepositoryLinks format for a single repo.
// This matches the output format of the current edges-linking module.
func TransformToRepositoryLinks(repo *models.RepositoryEdges, links []Link, logger *logrus.Logger) *models.RepositoryLinks {
	if repo == nil {
		return nil
	}

	repoName := repo.Name

	// Pass through API call chains
	apiCallChains := []models.APICallChain{}
	if repo.APICallChains != nil {
		apiCallChains = repo.APICallChains
	}

	result := &models.RepositoryLinks{
		RepositoryName: repoName,
		Calls:          []models.CallWithLink{},
		Providers:      []models.ProviderWithCallers{},
		APICallChains:  apiCallChains,
	}

	// Build calls map
	callsMap := make(map[string]*models.CallWithLink)

	// Add API calls
	for _, apiCall := range repo.APICalls {
		// Filter out calls from other repositories (data contamination check)
		if apiCall.Repo != "" && apiCall.Repo != repoName {
			if logger != nil {
				logger.WithFields(logrus.Fields{
					"current_repo": repoName,
					"call_repo":    apiCall.Repo,
					"identifier":   apiCall.Identifier,
				}).Warn("[non-llm-linking] Filtered out API call from different repository")
			}
			continue
		}

		key := fmt.Sprintf("api_call|%s", apiCall.Identifier)
		callsMap[key] = &models.CallWithLink{
			Repository:  repoName,
			File:        apiCall.File,
			Line:        apiCall.Line,
			Type:        "api_call",
			Subtype:     apiCall.Subtype,
			Identifier:  apiCall.Identifier,
			Description: apiCall.Description,
			LinkedTo:    []models.LinkedItem{},
		}
	}

	// Add database calls
	for _, dbCall := range repo.DatabaseCalls {
		if dbCall.Repo != "" && dbCall.Repo != repoName {
			if logger != nil {
				logger.WithFields(logrus.Fields{
					"current_repo": repoName,
					"call_repo":    dbCall.Repo,
					"identifier":   dbCall.Identifier,
				}).Warn("[non-llm-linking] Filtered out database call from different repository")
			}
			continue
		}

		key := fmt.Sprintf("database_call|%s", dbCall.Identifier)
		callsMap[key] = &models.CallWithLink{
			Repository:  repoName,
			File:        dbCall.File,
			Line:        dbCall.Line,
			Type:        "database_call",
			Subtype:     dbCall.Subtype,
			Identifier:  dbCall.Identifier,
			Description: dbCall.Description,
			LinkedTo:    []models.LinkedItem{},
		}
	}

	// Add queue calls
	for _, queueCall := range repo.QueueCalls {
		if queueCall.Repo != "" && queueCall.Repo != repoName {
			if logger != nil {
				logger.WithFields(logrus.Fields{
					"current_repo": repoName,
					"call_repo":    queueCall.Repo,
					"identifier":   queueCall.Identifier,
				}).Warn("[non-llm-linking] Filtered out queue call from different repository")
			}
			continue
		}

		key := fmt.Sprintf("queue_call|%s", queueCall.Identifier)
		callsMap[key] = &models.CallWithLink{
			Repository:  repoName,
			File:        queueCall.File,
			Line:        queueCall.Line,
			Type:        "queue_call",
			Subtype:     queueCall.Subtype,
			Identifier:  queueCall.Identifier,
			Description: queueCall.Description,
			LinkedTo:    []models.LinkedItem{},
		}
	}

	// Add links to calls (outgoing links from this repo)
	linkedToSeen := make(map[string]map[string]bool) // callKey -> set of linkedItemKeys
	for _, link := range links {
		if link.FromRepo != repoName {
			continue
		}

		callType := link.LinkType + "_call"
		if link.LinkType == "api" {
			callType = "api_call"
		}
		key := fmt.Sprintf("%s|%s", callType, link.CallerIdentifier)

		if call, exists := callsMap[key]; exists {
			linkedItem := models.LinkedItem{
				Repository:  link.ToRepo,
				File:        link.ProviderFile,
				Line:        link.ProviderLine,
				Identifier:  link.ProviderIdentifier,
				Confidence:  formatConfidence(link.Confidence),
				Description: "",
			}
			itemKey := makeLinkedItemKey(linkedItem)
			if linkedToSeen[key] == nil {
				linkedToSeen[key] = make(map[string]bool)
			}
			if !linkedToSeen[key][itemKey] {
				linkedToSeen[key][itemKey] = true
				call.LinkedTo = append(call.LinkedTo, linkedItem)
			}
		}
	}

	// Build providers map
	providersMap := make(map[string]*models.ProviderWithCallers)

	for _, provider := range repo.APIProviders {
		if provider.Repo != "" && provider.Repo != repoName {
			if logger != nil {
				logger.WithFields(logrus.Fields{
					"current_repo":  repoName,
					"provider_repo": provider.Repo,
					"identifier":    provider.Identifier,
				}).Warn("[non-llm-linking] Filtered out API provider from different repository")
			}
			continue
		}

		key := fmt.Sprintf("%s|%s|%s", repoName, provider.File, provider.Identifier)
		providersMap[key] = &models.ProviderWithCallers{
			Repository:     repoName,
			File:           provider.File,
			Line:           provider.Line,
			Type:           "api_provider",
			Identifier:     provider.Identifier,
			Description:    provider.Description,
			RequestSchema:  provider.RequestSchema,
			ResponseSchema: provider.ResponseSchema,
			CalledBy:       []models.CallerItem{},
		}
	}

	// Add reverse links (callers of providers in this repo)
	calledBySeen := make(map[string]map[string]bool) // providerKey -> set of callerItemKeys
	for _, link := range links {
		if link.ToRepo != repoName || link.LinkType != "api" {
			continue
		}

		key := fmt.Sprintf("%s|%s|%s", repoName, link.ProviderFile, link.ProviderIdentifier)

		if provider, exists := providersMap[key]; exists {
			callerItem := models.CallerItem{
				Repository:  link.FromRepo,
				File:        link.CallerFile,
				Line:        link.CallerLine,
				Identifier:  link.CallerIdentifier,
				Confidence:  formatConfidence(link.Confidence),
				Description: "",
			}
			itemKey := makeCallerItemKey(callerItem)
			if calledBySeen[key] == nil {
				calledBySeen[key] = make(map[string]bool)
			}
			if !calledBySeen[key][itemKey] {
				calledBySeen[key][itemKey] = true
				provider.CalledBy = append(provider.CalledBy, callerItem)
			}
		} else {
			// Try normalized matching
			normalizedLinkID := normalizer.NormalizeIdentifier(link.ProviderIdentifier)
			for provKey, provider := range providersMap {
				normalizedProviderID := normalizer.NormalizeIdentifier(provider.Identifier)
				if normalizedProviderID == normalizedLinkID && provider.File == link.ProviderFile {
					callerItem := models.CallerItem{
						Repository:  link.FromRepo,
						File:        link.CallerFile,
						Line:        link.CallerLine,
						Identifier:  link.CallerIdentifier,
						Confidence:  formatConfidence(link.Confidence),
						Description: "",
					}
					itemKey := makeCallerItemKey(callerItem)
					if calledBySeen[provKey] == nil {
						calledBySeen[provKey] = make(map[string]bool)
					}
					if !calledBySeen[provKey][itemKey] {
						calledBySeen[provKey][itemKey] = true
						provider.CalledBy = append(provider.CalledBy, callerItem)
					}
					break
				}
			}
		}
	}

	// Convert maps to slices
	for _, call := range callsMap {
		result.Calls = append(result.Calls, *call)
	}

	for _, provider := range providersMap {
		result.Providers = append(result.Providers, *provider)
	}

	return result
}

// formatConfidence converts a float64 confidence to the string format used by the existing module.
func formatConfidence(confidence float64) string {
	return fmt.Sprintf("%.2f", confidence)
}

// makeCallerItemKey generates a unique key for a CallerItem to use in deduplication.
func makeCallerItemKey(item models.CallerItem) string {
	return fmt.Sprintf("%s|%s|%d|%s", item.Repository, item.File, item.Line, item.Identifier)
}

// makeLinkedItemKey generates a unique key for a LinkedItem to use in deduplication.
func makeLinkedItemKey(item models.LinkedItem) string {
	return fmt.Sprintf("%s|%s|%d|%s", item.Repository, item.File, item.Line, item.Identifier)
}

// PopulateReverseLinks populates the called_by field on providers based on linked_to from calls.
// This is used to ensure bidirectional links are established.
func PopulateReverseLinks(allRepoLinks []*models.RepositoryLinks, logger *logrus.Logger) {
	// Build a map of repo -> provider identifier -> provider object for quick lookup
	providerMap := make(map[string]map[string]*models.ProviderWithCallers)

	// calledBySeen tracks which CallerItems have already been added to each provider,
	// keyed by repo -> providerKey -> callerItemKey. Initialized from existing CalledBy entries.
	calledBySeen := make(map[string]map[string]map[string]bool)

	for _, repoLinks := range allRepoLinks {
		if repoLinks == nil {
			continue
		}
		repoName := repoLinks.RepositoryName
		providerMap[repoName] = make(map[string]*models.ProviderWithCallers)
		calledBySeen[repoName] = make(map[string]map[string]bool)

		for i := range repoLinks.Providers {
			provider := &repoLinks.Providers[i]
			key := fmt.Sprintf("%s|%s|%s", provider.Repository, provider.File, provider.Identifier)
			providerMap[repoName][key] = provider

			// Seed the seen map from existing CalledBy entries and deduplicate them
			calledBySeen[repoName][key] = make(map[string]bool)
			dedupedCalledBy := make([]models.CallerItem, 0, len(provider.CalledBy))
			for _, cb := range provider.CalledBy {
				cbKey := makeCallerItemKey(cb)
				if !calledBySeen[repoName][key][cbKey] {
					calledBySeen[repoName][key][cbKey] = true
					dedupedCalledBy = append(dedupedCalledBy, cb)
				}
			}
			provider.CalledBy = dedupedCalledBy
		}
	}

	totalReverseLinks := 0
	exactReverseMatches := 0
	normalizedReverseMatches := 0
	unmatchedReverseLinks := 0

	// Deduplicate existing LinkedTo entries on calls
	for _, repoLinks := range allRepoLinks {
		if repoLinks == nil {
			continue
		}
		for i := range repoLinks.Calls {
			call := &repoLinks.Calls[i]
			seen := make(map[string]bool)
			dedupedLinkedTo := make([]models.LinkedItem, 0, len(call.LinkedTo))
			for _, item := range call.LinkedTo {
				itemKey := makeLinkedItemKey(item)
				if !seen[itemKey] {
					seen[itemKey] = true
					dedupedLinkedTo = append(dedupedLinkedTo, item)
				}
			}
			call.LinkedTo = dedupedLinkedTo
		}
	}

	// Iterate through all calls and populate the reverse direction
	for _, callerRepoLinks := range allRepoLinks {
		if callerRepoLinks == nil {
			continue
		}

		for _, call := range callerRepoLinks.Calls {
			for _, linkedItem := range call.LinkedTo {
				targetRepo := linkedItem.Repository

				if providers, exists := providerMap[targetRepo]; exists {
					totalReverseLinks++

					// Try exact match first
					providerKey := fmt.Sprintf("%s|%s|%s", targetRepo, linkedItem.File, linkedItem.Identifier)
					if provider, found := providers[providerKey]; found {
						exactReverseMatches++
						callerItem := models.CallerItem{
							Repository:  call.Repository,
							File:        call.File,
							Line:        call.Line,
							Identifier:  call.Identifier,
							Description: call.Description,
							Confidence:  linkedItem.Confidence,
						}
						itemKey := makeCallerItemKey(callerItem)
						if calledBySeen[targetRepo] == nil {
							calledBySeen[targetRepo] = make(map[string]map[string]bool)
						}
						if calledBySeen[targetRepo][providerKey] == nil {
							calledBySeen[targetRepo][providerKey] = make(map[string]bool)
						}
						if !calledBySeen[targetRepo][providerKey][itemKey] {
							calledBySeen[targetRepo][providerKey][itemKey] = true
							provider.CalledBy = append(provider.CalledBy, callerItem)
						}
					} else {
						// Try normalized matching
						normalizedTargetID := normalizer.NormalizeIdentifier(linkedItem.Identifier)
						matched := false

						for provKey, provider := range providers {
							normalizedProviderID := normalizer.NormalizeIdentifier(provider.Identifier)
							if normalizedProviderID == normalizedTargetID && provider.File == linkedItem.File {
								normalizedReverseMatches++
								matched = true
								callerItem := models.CallerItem{
									Repository:  call.Repository,
									File:        call.File,
									Line:        call.Line,
									Identifier:  call.Identifier,
									Description: call.Description,
									Confidence:  linkedItem.Confidence,
								}
								itemKey := makeCallerItemKey(callerItem)
								if calledBySeen[targetRepo] == nil {
									calledBySeen[targetRepo] = make(map[string]map[string]bool)
								}
								if calledBySeen[targetRepo][provKey] == nil {
									calledBySeen[targetRepo][provKey] = make(map[string]bool)
								}
								if !calledBySeen[targetRepo][provKey][itemKey] {
									calledBySeen[targetRepo][provKey][itemKey] = true
									provider.CalledBy = append(provider.CalledBy, callerItem)
								}
							}
						}

						if !matched {
							unmatchedReverseLinks++
						}
					}
				}
			}
		}
	}

	if logger != nil {
		logger.WithFields(logrus.Fields{
			"total_reverse_links":        totalReverseLinks,
			"exact_reverse_matches":      exactReverseMatches,
			"normalized_reverse_matches": normalizedReverseMatches,
			"unmatched_reverse_links":    unmatchedReverseLinks,
		}).Info("[non-llm-linking] Reverse linking (called_by) summary")
	}
}
