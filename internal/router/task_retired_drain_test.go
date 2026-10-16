package router

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/middleware/asynqdl"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

// Retired task types — types an older release enqueued and this build no
// longer implements — are a specific defect class: the queue keeps being
// consumed, so a pre-upgrade backlog keeps arriving, but the mux has no
// handler. asynq then answers ErrHandlerNotFound on every delivery, retries
// the task until it dead-letters, and spends shared worker capacity on it the
// whole time. These tests pin the chosen strategy (see
// types.RetiredTaskTypes): drain on first delivery, no retry, no dead letter,
// no work — while genuinely unknown task types keep the old error policy.

// recordingDeadLetterRepo records every Insert so a test can assert that a
// drained task never produced a dead letter.
type recordingDeadLetterRepo struct {
	mu   sync.Mutex
	rows []*types.TaskDeadLetter
}

func (r *recordingDeadLetterRepo) Insert(_ context.Context, dl *types.TaskDeadLetter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *dl
	r.rows = append(r.rows, &cp)
	return nil
}

func (r *recordingDeadLetterRepo) ListByScope(context.Context, string, string, string, int) ([]*types.TaskDeadLetter, string, error) {
	return nil, "", nil
}

func (r *recordingDeadLetterRepo) ListByTaskType(context.Context, string, string, int) ([]*types.TaskDeadLetter, string, error) {
	return nil, "", nil
}

func (r *recordingDeadLetterRepo) DeleteByID(context.Context, int64) error { return nil }

func (r *recordingDeadLetterRepo) taskTypes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make([]string, 0, len(r.rows))
	for _, row := range r.rows {
		seen = append(seen, row.TaskType)
	}
	return seen
}

// TestRetiredTaskDrainsThroughMuxWithoutDeadLetter drives the real mux the
// worker runs — with the same dead-letter middleware chain RunAsynqServer
// installs — using the payload shape an older release would have enqueued. The
// task must complete on first delivery and must leave no dead-letter row.
func TestRetiredTaskDrainsThroughMuxWithoutDeadLetter(t *testing.T) {
	require.NotEmpty(t, types.RetiredTaskTypes)

	repo := &recordingDeadLetterRepo{}
	mux := asynq.NewServeMux()
	mux.Use(asynqdl.MiddlewareWithCallback(repo, nil))
	registerRetiredTaskHandlers(mux.HandleFunc)

	// A payload the retired handler would have rejected (no subject, no
	// session): draining must not depend on decoding it.
	payload, err := json.Marshal(types.MemoryExtractPayload{TenantID: 4242, MessageID: "message-1"})
	require.NoError(t, err)

	before := DrainedRetiredTaskCount()
	for _, retiredType := range types.RetiredTaskTypes {
		task := asynq.NewTask(retiredType, payload)
		require.NoErrorf(t, mux.ProcessTask(context.Background(), task),
			"%s must complete on its first delivery, so asynq neither retries nor dead-letters it", retiredType)
		// A redelivery (asynq retry, or an operator requeueing an archived
		// task) behaves the same way.
		require.NoErrorf(t, mux.ProcessTask(context.Background(), task),
			"%s must stay inert on redelivery", retiredType)
		// asynq matches patterns by longest prefix, so a variant of a retired
		// type lands on the same drain handler rather than on
		// ErrHandlerNotFound. Worth pinning: it is the reason a retired family
		// (memory:extract.*) cannot leak back into retry storms.
		variant := asynq.NewTask(retiredType+".legacy", payload)
		require.NoErrorf(t, mux.ProcessTask(context.Background(), variant),
			"%s must be drained by the same handler", variant.Type())
	}
	require.Equal(t, before+3*uint64(len(types.RetiredTaskTypes)), DrainedRetiredTaskCount(),
		"every drained retired task must be counted")
	require.Emptyf(t, repo.taskTypes(),
		"a drained retired task must not produce a dead letter; got rows for %v", repo.taskTypes())
}

// TestUnknownTaskTypeKeepsHandlerNotFoundPolicy is the other side of the
// guard: the drain must not become a catch-all. A task type that matches no
// registered pattern keeps asynq's existing failure policy —
// ErrHandlerNotFound, and the dead-letter row that follows from it.
func TestUnknownTaskTypeKeepsHandlerNotFoundPolicy(t *testing.T) {
	repo := &recordingDeadLetterRepo{}
	mux := asynq.NewServeMux()
	mux.Use(asynqdl.MiddlewareWithCallback(repo, nil))
	registerRetiredTaskHandlers(mux.HandleFunc)

	// Deliberately unrelated to any registered or retired type: a name that
	// merely extends a retired one is drained on purpose (see above).
	unknown := "legacy:unregistered"
	for _, retiredType := range types.RetiredTaskTypes {
		require.NotEqual(t, retiredType, unknown)
		require.Falsef(t, strings.HasPrefix(unknown, retiredType),
			"the unknown type must not fall under the %q pattern", retiredType)
	}

	err := mux.ProcessTask(context.Background(), asynq.NewTask(unknown, []byte(`{"tenant_id":7}`)))
	require.ErrorIsf(t, err, asynq.ErrHandlerNotFound,
		"an unregistered task type must keep failing with ErrHandlerNotFound, not be swallowed")
	require.Equal(t, []string{unknown}, repo.taskTypes(),
		"the failure must still reach the dead-letter path")
}

// TestDrainGuardLeavesLiveTaskTypesAlone pins the blast radius of the guard.
// The retired patterns are the only thing registerRetiredTaskHandlers adds, so
// a live pipeline type is still "not found" in a mux that carries them alone
// (in the real mux its own handler answers it), and it must not move the drain
// counter.
func TestDrainGuardLeavesLiveTaskTypesAlone(t *testing.T) {
	repo := &recordingDeadLetterRepo{}
	mux := asynq.NewServeMux()
	mux.Use(asynqdl.MiddlewareWithCallback(repo, nil))
	registerRetiredTaskHandlers(mux.HandleFunc)

	liveTypes := []string{
		types.TypeDocumentProcess,
		types.TypeKnowledgePostProcess,
		types.TypeChunkExtract,
		types.TypeWikiIngest,
		types.TypeWikiFinalize,
		types.TypeSummaryGeneration,
	}
	for _, liveType := range liveTypes {
		before := DrainedRetiredTaskCount()
		err := mux.ProcessTask(context.Background(), asynq.NewTask(liveType, []byte(`{"tenant_id":7,"knowledge_id":"k1"}`)))
		require.ErrorIsf(t, err, asynq.ErrHandlerNotFound,
			"%s must not be served by the retired drain handler", liveType)
		require.Equalf(t, before, DrainedRetiredTaskCount(),
			"%s must not be counted as a drained retired task", liveType)
	}
}

// TestSyncExecutorDrainsRetiredTasks covers the Lite execution path: with no
// Redis there is no backlog to drain, but a task enqueued in the same process
// must not fail with "no handler registered for type".
func TestSyncExecutorDrainsRetiredTasks(t *testing.T) {
	executor := NewSyncTaskExecutor()
	registerRetiredTaskHandlers(executor.RegisterHandler)

	before := DrainedRetiredTaskCount()
	for _, retiredType := range types.RetiredTaskTypes {
		info, err := executor.Enqueue(asynq.NewTask(retiredType, []byte(`{"tenant_id":4242}`)))
		require.NoErrorf(t, err, "%s must be accepted by the Lite executor", retiredType)
		require.Equal(t, retiredType, info.Type)
	}
	require.Eventuallyf(t, func() bool {
		return DrainedRetiredTaskCount() == before+uint64(len(types.RetiredTaskTypes))
	}, 5*time.Second, 10*time.Millisecond, "the Lite executor must run the drain handler for each retired type")
}

// TestEveryConsumedTaskTypeHasAHandler is the drift guard: it is the check
// whose absence let `memory:extract` sit in a consumed queue with no handler
// at all. Every task type declared in the queue topology must be registered
// literally on both execution paths or covered by the retired drain registry,
// and every retired type must still be consumed — otherwise the drain handler
// is dead code and the legacy backlog it exists for would sit in Redis
// forever.
//
// The registrations are read from the sources rather than from a
// hand-maintained map in this test, so a new task type cannot slip through by
// being forgotten here as well.
func TestEveryConsumedTaskTypeHasAHandler(t *testing.T) {
	typeConstRe := regexp.MustCompile(`(?m)^\s*(Type[A-Za-z0-9]+)\s*=\s*"([^"]+)"`)
	asynqRegistrationRe := regexp.MustCompile(`mux\.HandleFunc\(types\.(Type[A-Za-z0-9]+),`)
	syncRegistrationRe := regexp.MustCompile(`RegisterHandler\(types\.(Type[A-Za-z0-9]+),`)

	nameToValue := map[string]string{}
	for _, match := range typeConstRe.FindAllStringSubmatch(readFile(t, "..", "types", "task.go"), -1) {
		nameToValue[match[1]] = match[2]
	}
	require.NotEmpty(t, nameToValue, "types/task.go must declare the task type constants")

	declared := map[string]bool{}
	for _, definition := range types.QueueDefinitions() {
		for _, taskType := range definition.TaskTypes {
			declared[taskType] = true
		}
	}
	require.NotEmpty(t, declared, "the queue topology must declare task types")

	retired := map[string]bool{}
	for _, retiredType := range types.RetiredTaskTypes {
		require.Truef(t, declared[retiredType],
			"retired task type %q is no longer consumed by any queue: the drain handler can never run, so "+
				"either restore the queue definition or drop the registry entry together with its payload type",
			retiredType)
		retired[retiredType] = true
	}

	for _, path := range []struct {
		file string
		re   *regexp.Regexp
	}{
		{"task.go", asynqRegistrationRe},
		{"sync_task.go", syncRegistrationRe},
	} {
		source := readFile(t, ".", path.file)
		require.Containsf(t, source, "registerRetiredTaskHandlers(",
			"%s must install the retired drain handlers", path.file)

		registered := map[string]bool{}
		matches := path.re.FindAllStringSubmatch(source, -1)
		require.NotEmptyf(t, matches, "%s must register task handlers", path.file)
		for _, match := range matches {
			value, ok := nameToValue[match[1]]
			require.Truef(t, ok, "%s registers types.%s, which types/task.go does not declare", path.file, match[1])
			registered[value] = true
		}

		for taskType := range declared {
			if retired[taskType] {
				// Registering a retired type literally as well would panic at
				// startup: asynq refuses a duplicate pattern.
				require.Falsef(t, registered[taskType],
					"%s registers %q literally although it is in types.RetiredTaskTypes, which registers it too — "+
						"asynq panics on a duplicate pattern", path.file, taskType)
				continue
			}
			require.Truef(t, registered[taskType],
				"task type %q is consumed by the queue topology but %s registers no handler for it — that is "+
					"the memory:extract defect: ErrHandlerNotFound on every delivery, ending in the dead letter",
				taskType, path.file)
		}
		for taskType := range registered {
			require.Truef(t, declared[taskType],
				"%s registers a handler for %q, which no queue definition carries, so the task can never arrive",
				path.file, taskType)
		}
	}
}

// readFile reads a path relative to the router package directory, so the
// assertions above run against the code that ships in this package.
func readFile(t *testing.T, elems ...string) string {
	t.Helper()
	path := filepath.Join(elems...)
	content, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read %s", path)
	return string(content)
}
