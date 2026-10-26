package journalstore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// EntityTypeRow is one row of kanban-store's GET /api/entity-types, the
// fields this service reads. kanban-store owns the registry; this is a copy
// for classifying ids, never a second list.
type EntityTypeRow struct {
	Type       string   `json:"type"`
	Service    string   `json:"service"`
	Get        string   `json:"get"`
	IDPatterns []string `json:"id_patterns"`
}

type compiledEntityType struct {
	entityType string
	patterns   []*regexp.Regexp
}

// EntityTypeRegistry classifies a ref by the id patterns in kanban-store's
// registry, anchored and case-insensitive, the way dash's resolver reads them.
//
// It never blocks a write. Until the first read succeeds it classifies
// nothing, and a mention is stored with an empty type; Store.BackfillMention
// Types fills those in after a later read. A failed read keeps the last good
// copy and is reported by Status, which GET /health serves.
type EntityTypeRegistry struct {
	fetch func() ([]EntityTypeRow, error)

	mutex     sync.RWMutex
	types     []compiledEntityType
	fetchedAt time.Time
	lastError error
}

// NewKanbanEntityTypeRegistry reads the registry from kanban-store with the
// internal service token. Nothing is read until Refresh.
func NewKanbanEntityTypeRegistry(kanbanStoreURL, serviceToken string) *EntityTypeRegistry {
	client := &http.Client{Timeout: 10 * time.Second}
	return &EntityTypeRegistry{fetch: func() ([]EntityTypeRow, error) {
		request, err := http.NewRequest(http.MethodGet, kanbanStoreURL+"/api/entity-types", nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("X-Kanban-Store-Service-Token", serviceToken)
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("kanban-store entity-type registry unreachable: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("kanban-store entity-type registry answered %d", response.StatusCode)
		}
		var rows []EntityTypeRow
		if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
			return nil, fmt.Errorf("kanban-store entity-type registry unparseable: %w", err)
		}
		return rows, nil
	}}
}

// NewStaticEntityTypeRegistry classifies by rows given in code. For tests.
func NewStaticEntityTypeRegistry(rows []EntityTypeRow) *EntityTypeRegistry {
	return &EntityTypeRegistry{fetch: func() ([]EntityTypeRow, error) { return rows, nil }}
}

// Refresh reads the registry again. On failure the previous copy stays in use
// and the error is kept for Status.
func (r *EntityTypeRegistry) Refresh() error {
	rows, err := r.fetch()
	if err == nil {
		var compiled []compiledEntityType
		compiled, err = compileEntityTypes(rows)
		if err == nil {
			r.mutex.Lock()
			r.types = compiled
			r.fetchedAt = time.Now()
			r.lastError = nil
			r.mutex.Unlock()
			return nil
		}
	}
	r.mutex.Lock()
	r.lastError = err
	r.mutex.Unlock()
	return err
}

// compileEntityTypes refuses the whole registry when one pattern does not
// compile, rather than classifying with a silently shorter list.
func compileEntityTypes(rows []EntityTypeRow) ([]compiledEntityType, error) {
	compiled := make([]compiledEntityType, 0, len(rows))
	for _, row := range rows {
		if len(row.IDPatterns) == 0 {
			continue
		}
		entry := compiledEntityType{entityType: row.Type}
		for _, pattern := range row.IDPatterns {
			expression, err := regexp.Compile(`(?i)^(?:` + pattern + `)$`)
			if err != nil {
				return nil, fmt.Errorf("entity type %q: id pattern %q does not compile: %w", row.Type, pattern, err)
			}
			entry.patterns = append(entry.patterns, expression)
		}
		compiled = append(compiled, entry)
	}
	return compiled, nil
}

// Classify names the one registry type whose pattern matches ref, or "" when
// none or several do. Several matches are left for the reader to resolve, the
// way dash's resolver reports every match rather than picking one.
func (r *EntityTypeRegistry) Classify(ref string) string {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	found := ""
	for _, entityType := range r.types {
		for _, pattern := range entityType.patterns {
			if pattern.MatchString(ref) {
				if found != "" && found != entityType.entityType {
					return ""
				}
				found = entityType.entityType
				break
			}
		}
	}
	return found
}

// Status reports how many typed rows are in use, when they were read, and the
// last read's error, if it failed.
func (r *EntityTypeRegistry) Status() EntityRegistryStatus {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	status := EntityRegistryStatus{Types: len(r.types)}
	if !r.fetchedAt.IsZero() {
		status.FetchedAt = r.fetchedAt.Unix()
	}
	if r.lastError != nil {
		status.Error = r.lastError.Error()
	} else if r.fetchedAt.IsZero() {
		status.Error = "the registry has not been read yet"
	}
	return status
}
