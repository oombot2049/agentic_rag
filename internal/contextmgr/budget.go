package contextmgr

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrUnknownModel      = errors.New("token budget unknown model")
	ErrMandatoryOverflow = errors.New("token budget mandatory content overflow")
	ErrTokenizer         = errors.New("token budget tokenizer failure")
)

type Tokenizer interface {
	Count(string) (int, error)
	Truncate(string, int) (string, error)
}

type Model struct {
	ID           string
	WindowTokens int
	Tokenizer    Tokenizer
}

type Registry struct{ models map[string]Model }

func NewRegistry(models []Model) (*Registry, error) {
	result := &Registry{models: make(map[string]Model, len(models))}
	for _, model := range models {
		if strings.TrimSpace(model.ID) != model.ID || model.ID == "" || model.WindowTokens < 1 || model.Tokenizer == nil {
			return nil, fmt.Errorf("%w: invalid model registration", ErrInvalidArgument)
		}
		if _, duplicate := result.models[model.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate model", ErrInvalidArgument)
		}
		result.models[model.ID] = model
	}
	if len(result.models) == 0 {
		return nil, fmt.Errorf("%w: empty model registry", ErrInvalidArgument)
	}
	return result, nil
}

func (r *Registry) Resolve(id string) (Model, error) {
	if r == nil {
		return Model{}, ErrUnknownModel
	}
	model, ok := r.models[id]
	if !ok {
		return Model{}, fmt.Errorf("%w: %s", ErrUnknownModel, id)
	}
	return model, nil
}

type BudgetConfig struct {
	SafetyMarginTokens  int
	EntryOverheadTokens int
}

func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{SafetyMarginTokens: 64, EntryOverheadTokens: 4}
}

type Evidence struct {
	ID      string
	Content string
}

type BudgetRequest struct {
	ModelID              string
	ReservedOutputTokens int
	Snapshot             Snapshot
	RAGEvidence          []Evidence
}

type DecisionAction string

const (
	DecisionKept      DecisionAction = "KEPT"
	DecisionDropped   DecisionAction = "DROPPED"
	DecisionTruncated DecisionAction = "TRUNCATED"
)

type Decision struct {
	Key            string
	Source         Source
	Action         DecisionAction
	OriginalTokens int
	FinalTokens    int
	Reason         string
}

type Plan struct {
	modelID        string
	window         int
	inputLimit     int
	inputUsed      int
	reservedOutput int
	safetyMargin   int
	entries        []Entry
	decisions      []Decision
}

func (p Plan) ModelID() string           { return p.modelID }
func (p Plan) WindowTokens() int         { return p.window }
func (p Plan) InputLimitTokens() int     { return p.inputLimit }
func (p Plan) InputUsedTokens() int      { return p.inputUsed }
func (p Plan) ReservedOutputTokens() int { return p.reservedOutput }
func (p Plan) SafetyMarginTokens() int   { return p.safetyMargin }
func (p Plan) Entries() []Entry          { return append([]Entry(nil), p.entries...) }
func (p Plan) Decisions() []Decision     { return append([]Decision(nil), p.decisions...) }

type Budgeter struct {
	registry *Registry
	config   BudgetConfig
}

func NewBudgeter(registry *Registry, configuration BudgetConfig) (*Budgeter, error) {
	if registry == nil || configuration.SafetyMarginTokens < 0 || configuration.EntryOverheadTokens < 0 {
		return nil, fmt.Errorf("%w: token budget configuration", ErrInvalidArgument)
	}
	return &Budgeter{registry: registry, config: configuration}, nil
}

type budgetItem struct {
	entry          Entry
	key            string
	tokens         int
	originalTokens int
	selected       bool
	action         DecisionAction
	reason         string
}

func (b *Budgeter) Apply(request BudgetRequest) (Plan, error) {
	model, err := b.registry.Resolve(request.ModelID)
	if err != nil {
		return Plan{}, err
	}
	if request.ReservedOutputTokens < 1 || request.ReservedOutputTokens+b.config.SafetyMarginTokens >= model.WindowTokens {
		return Plan{}, fmt.Errorf("%w: output reservation leaves no input capacity", ErrMandatoryOverflow)
	}
	items, err := b.items(request, model.Tokenizer)
	if err != nil {
		return Plan{}, err
	}
	limit := model.WindowTokens - request.ReservedOutputTokens - b.config.SafetyMarginTokens
	used, err := b.admitMandatory(items, limit, model.Tokenizer)
	if err != nil {
		return Plan{}, err
	}
	used = b.admitHistory(items, used, limit)
	used, err = b.admitOptional(items, used, limit, model.Tokenizer)
	if err != nil {
		return Plan{}, err
	}
	entries, decisions := materialize(items)
	if used > limit {
		return Plan{}, fmt.Errorf("%w: invariant violated", ErrTokenizer)
	}
	return Plan{modelID: model.ID, window: model.WindowTokens, inputLimit: limit, inputUsed: used,
		reservedOutput: request.ReservedOutputTokens, safetyMargin: b.config.SafetyMarginTokens,
		entries: entries, decisions: decisions}, nil
}

func (b *Budgeter) items(request BudgetRequest, tokenizer Tokenizer) ([]*budgetItem, error) {
	entries := request.Snapshot.Entries()
	insertAt := len(entries)
	for index, entry := range entries {
		if entry.Source == SourceCurrentUser {
			insertAt = index
			break
		}
	}
	seenEvidence := make(map[string]struct{}, len(request.RAGEvidence))
	for _, evidence := range request.RAGEvidence {
		if strings.TrimSpace(evidence.ID) != evidence.ID || evidence.ID == "" || !validContent(evidence.Content) {
			return nil, fmt.Errorf("%w: RAG evidence", ErrInvalidArgument)
		}
		if _, duplicate := seenEvidence[evidence.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate RAG evidence", ErrInvalidArgument)
		}
		seenEvidence[evidence.ID] = struct{}{}
		entry := Entry{Source: SourceRAG, Priority: PriorityRAG, Role: "SYSTEM", ReferenceID: evidence.ID, Content: evidence.Content}
		entries = append(entries, Entry{})
		copy(entries[insertAt+1:], entries[insertAt:])
		entries[insertAt] = entry
		insertAt++
	}
	items := make([]*budgetItem, len(entries))
	for index, entry := range entries {
		count, err := countEntry(tokenizer, entry.Content, b.config.EntryOverheadTokens)
		if err != nil {
			return nil, err
		}
		items[index] = &budgetItem{entry: entry, key: entryKey(entry, index), tokens: count, originalTokens: count,
			action: DecisionDropped, reason: "insufficient_budget"}
	}
	return items, nil
}

func (b *Budgeter) admitMandatory(items []*budgetItem, limit int, tokenizer Tokenizer) (int, error) {
	used := 0
	for _, item := range items {
		if item.entry.Source != SourceSystem {
			continue
		}
		if used+item.tokens > limit {
			return 0, ErrMandatoryOverflow
		}
		selectItem(item, DecisionKept, "mandatory_system")
		used += item.tokens
	}
	for _, item := range items {
		if item.entry.Source != SourceCurrentUser {
			continue
		}
		if used+item.tokens <= limit {
			selectItem(item, DecisionKept, "mandatory_current_user")
			return used + item.tokens, nil
		}
		available := limit - used - b.config.EntryOverheadTokens
		if available < 1 {
			return 0, ErrMandatoryOverflow
		}
		if err := truncateItem(item, tokenizer, available, b.config.EntryOverheadTokens, "current_user_window_limit"); err != nil {
			return 0, err
		}
		return used + item.tokens, nil
	}
	return 0, ErrMandatoryOverflow
}

func (b *Budgeter) admitHistory(items []*budgetItem, used, limit int) int {
	groups := make(map[uint64][]*budgetItem)
	var sequences []uint64
	for _, item := range items {
		if item.entry.Source != SourceHistory {
			continue
		}
		if _, exists := groups[item.entry.TurnSequence]; !exists {
			sequences = append(sequences, item.entry.TurnSequence)
		}
		groups[item.entry.TurnSequence] = append(groups[item.entry.TurnSequence], item)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] > sequences[j] })
	for _, sequence := range sequences {
		group, total := groups[sequence], 0
		for _, item := range group {
			total += item.tokens
		}
		if len(group) == 2 && used+total <= limit {
			for _, item := range group {
				selectItem(item, DecisionKept, "recent_complete_turn")
			}
			used += total
		} else {
			for _, item := range group {
				item.reason = "history_pair_does_not_fit"
			}
		}
	}
	return used
}

func (b *Budgeter) admitOptional(items []*budgetItem, used, limit int, tokenizer Tokenizer) (int, error) {
	for _, source := range []Source{SourceMemory, SourceRAG} {
		for _, item := range items {
			if item.entry.Source != source {
				continue
			}
			if used+item.tokens <= limit {
				selectItem(item, DecisionKept, "priority_lane_fit")
				used += item.tokens
				continue
			}
			available := limit - used - b.config.EntryOverheadTokens
			if available > 0 {
				if err := truncateItem(item, tokenizer, available, b.config.EntryOverheadTokens, "priority_lane_partial_fit"); err != nil {
					return 0, err
				}
				used += item.tokens
			}
			return used, nil
		}
	}
	return used, nil
}

func countEntry(tokenizer Tokenizer, content string, overhead int) (int, error) {
	count, err := tokenizer.Count(content)
	if err != nil || count < 1 {
		return 0, fmt.Errorf("%w: count", ErrTokenizer)
	}
	return count + overhead, nil
}

func truncateItem(item *budgetItem, tokenizer Tokenizer, contentLimit, overhead int, reason string) error {
	content, err := tokenizer.Truncate(item.entry.Content, contentLimit)
	if err != nil || strings.TrimSpace(content) == "" {
		return fmt.Errorf("%w: truncate", ErrTokenizer)
	}
	count, err := tokenizer.Count(content)
	if err != nil || count < 1 || count > contentLimit {
		return fmt.Errorf("%w: truncation exceeded requested limit", ErrTokenizer)
	}
	item.entry.Content, item.tokens, item.selected, item.action, item.reason = content, count+overhead, true, DecisionTruncated, reason
	return nil
}

func selectItem(item *budgetItem, action DecisionAction, reason string) {
	item.selected, item.action, item.reason = true, action, reason
}

func materialize(items []*budgetItem) ([]Entry, []Decision) {
	entries := make([]Entry, 0, len(items))
	decisions := make([]Decision, 0, len(items))
	for _, item := range items {
		final := 0
		if item.selected {
			entries = append(entries, item.entry)
			final = item.tokens
		}
		decisions = append(decisions, Decision{Key: item.key, Source: item.entry.Source, Action: item.action,
			OriginalTokens: item.originalTokens, FinalTokens: final, Reason: item.reason})
	}
	return entries, decisions
}

func entryKey(entry Entry, index int) string {
	identifier := entry.ReferenceID
	if entry.MessageID != "" {
		identifier = entry.MessageID.String()
	}
	if identifier == "" {
		identifier = fmt.Sprintf("entry-%03d", index+1)
	}
	return string(entry.Source) + ":" + identifier
}
