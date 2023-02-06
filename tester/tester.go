package tester

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/cschleiden/go-workflows/internal/activity"
	margs "github.com/cschleiden/go-workflows/internal/args"
	"github.com/cschleiden/go-workflows/internal/command"
	"github.com/cschleiden/go-workflows/internal/converter"
	"github.com/cschleiden/go-workflows/internal/core"
	"github.com/cschleiden/go-workflows/internal/fn"
	"github.com/cschleiden/go-workflows/internal/history"
	"github.com/cschleiden/go-workflows/internal/logger"
	"github.com/cschleiden/go-workflows/internal/payload"
	"github.com/cschleiden/go-workflows/internal/task"
	"github.com/cschleiden/go-workflows/internal/workflow"
	"github.com/cschleiden/go-workflows/log"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"go.opentelemetry.io/otel/trace"
)

type testHistoryProvider struct {
	history []history.Event
}

func (t *testHistoryProvider) GetWorkflowInstanceHistory(ctx context.Context, instance *core.WorkflowInstance, lastSequenceID *int64) ([]history.Event, error) {
	return t.history, nil
}

type WorkflowTester[TResult any] interface {
	// Now returns the current time of the simulated clock in the tester
	Now() time.Time

	Execute(args ...interface{})

	Registry() *workflow.Registry

	OnActivity(activity interface{}, args ...interface{}) *mock.Call

	OnSubWorkflow(workflow interface{}, args ...interface{}) *mock.Call

	SignalWorkflow(signalName string, value interface{})

	SignalWorkflowInstance(wfi *core.WorkflowInstance, signalName string, value interface{})

	WorkflowFinished() bool

	WorkflowResult() (TResult, string)

	// AssertExpectations asserts any assertions set up for mock activities and sub-workflow
	AssertExpectations(t *testing.T)

	// ScheduleCallback schedules the given callback after the given delay in workflow time (not wall clock).
	ScheduleCallback(delay time.Duration, callback func())

	// ListenSubWorkflow registers a handler to be called when a sub-workflow is started.
	ListenSubWorkflow(listener func(instance *core.WorkflowInstance, name string))
}

type testTimer struct {
	// At is the timer this timer is scheduled for. This will advance the mock clock
	// to this timestamp
	At time.Time

	// Callback is called when the timer should fire. It can return a history event which
	// will be added to the event history being executed.
	Callback func()
}

type testWorkflow struct {
	instance      *core.WorkflowInstance
	history       []history.Event
	pendingEvents []history.Event
}

type options struct {
	TestTimeout time.Duration
	Logger      log.Logger
}

type workflowTester[TResult any] struct {
	options *options

	// Workflow under test
	wf  interface{}
	wfi *core.WorkflowInstance

	// Workflows
	testWorkflows []*testWorkflow

	workflowFinished bool
	workflowResult   payload.Payload
	workflowErr      string

	registry *workflow.Registry

	ma               *mock.Mock
	mockedActivities map[string]bool

	mw              *mock.Mock
	mockedWorkflows map[string]bool

	workflowHistory []history.Event
	clock           *clock.Mock

	timers    []*testTimer
	callbacks chan func() *history.WorkflowEvent

	subWorkflowListener func(*core.WorkflowInstance, string)

	runningActivities int32

	logger log.Logger

	tracer trace.Tracer
}

type WorkflowTesterOption func(*options)

func WithLogger(logger log.Logger) WorkflowTesterOption {
	return func(o *options) {
		o.Logger = logger
	}
}

func WithTestTimeout(timeout time.Duration) WorkflowTesterOption {
	return func(o *options) {
		o.TestTimeout = timeout
	}
}

func NewWorkflowTester[TResult any](wf interface{}, opts ...WorkflowTesterOption) WorkflowTester[TResult] {
	// Start with the current wall-clock tiem
	clock := clock.NewMock()
	clock.Set(time.Now())

	wfi := core.NewWorkflowInstance(uuid.NewString(), uuid.NewString())
	registry := workflow.NewRegistry()

	options := &options{
		TestTimeout: time.Second * 10,
	}

	for _, o := range opts {
		o(options)
	}

	if options.Logger == nil {
		options.Logger = logger.NewDefaultLogger()
	}

	tracer := trace.NewNoopTracerProvider().Tracer("workflow-tester")

	wt := &workflowTester[TResult]{
		options: options,

		wf:       wf,
		wfi:      wfi,
		registry: registry,

		testWorkflows: make([]*testWorkflow, 0),

		ma:               &mock.Mock{},
		mockedActivities: make(map[string]bool),

		mw:              &mock.Mock{},
		mockedWorkflows: make(map[string]bool),

		workflowHistory: make([]history.Event, 0),
		clock:           clock,

		timers:    make([]*testTimer, 0),
		callbacks: make(chan func() *history.WorkflowEvent, 1024),

		logger: options.Logger,
		tracer: tracer,
	}

	// Always register the workflow under test
	if err := wt.registry.RegisterWorkflow(wf); err != nil {
		panic(fmt.Sprintf("could not workflow under test: %v", err))
	}

	return wt
}

func (wt *workflowTester[TResult]) Now() time.Time {
	return wt.clock.Now()
}

func (wt *workflowTester[TResult]) Registry() *workflow.Registry {
	return wt.registry
}

func (wt *workflowTester[TResult]) ScheduleCallback(delay time.Duration, callback func()) {
	wt.timers = append(wt.timers, &testTimer{
		At:       wt.clock.Now().Add(delay),
		Callback: callback,
	})
}

func (wt *workflowTester[TResult]) ListenSubWorkflow(listener func(*core.WorkflowInstance, string)) {
	wt.subWorkflowListener = listener
}

func (wt *workflowTester[TResult]) OnActivity(activity interface{}, args ...interface{}) *mock.Call {
	// Register activity so that we can correctly identify its arguments later
	wt.registry.RegisterActivity(activity)

	name := fn.Name(activity)
	wt.mockedActivities[name] = true
	return wt.ma.On(name, args...)
}

func (wt *workflowTester[TResult]) OnSubWorkflow(workflow interface{}, args ...interface{}) *mock.Call {
	// Register workflow so that we can correctly identify its arguments later
	wt.registry.RegisterWorkflow(workflow)

	name := fn.Name(workflow)
	wt.mockedWorkflows[name] = true
	return wt.mw.On(name, args...)
}

func (wt *workflowTester[TResult]) Execute(args ...interface{}) {
	// Start workflow under test
	wt.testWorkflows = append(wt.testWorkflows, &testWorkflow{
		instance:      wt.wfi,
		pendingEvents: []history.Event{wt.getInitialEvent(wt.wf, args)},
		history:       make([]history.Event, 0),
	})

	for !wt.workflowFinished {
		// Execute all workflows until no more events?
		gotNewEvents := false

		for _, tw := range wt.testWorkflows {
			if len(tw.pendingEvents) == 0 {
				// Nothing to process for this workflow
				continue
			}

			// Get task
			t := getNextWorkflowTask(tw.instance, tw.history, tw.pendingEvents)
			tw.pendingEvents = tw.pendingEvents[:0]

			// Execute task
			e, err := workflow.NewExecutor(wt.logger, wt.tracer, wt.registry, &testHistoryProvider{tw.history}, tw.instance, wt.clock)
			if err != nil {
				panic("could not create workflow executor" + err.Error())
			}

			result, err := e.ExecuteTask(context.Background(), t)
			if err != nil {
				panic("Error while executing workflow" + err.Error())
			}

			e.Close()

			// Add all executed events to history
			tw.history = append(tw.history, result.Executed...)

			for _, event := range result.Executed {
				wt.logger.Debug("Event", "event_type", event.Type)

				switch event.Type {
				case history.EventType_WorkflowExecutionFinished:
					a := event.Attributes.(*history.ExecutionCompletedAttributes)

					if !tw.instance.SubWorkflow() {
						wt.workflowFinished = true
						wt.workflowResult = a.Result
						wt.workflowErr = a.Error
					}
				}
			}

			// Schedule activities
			for _, event := range result.ActivityEvents {
				wt.scheduleActivity(tw.instance, event)
			}

			// Ensure WorkflowInstance corresponding to each WorkflowEvent has an updated
			// ParentInstanceID and ParentEventID. This prevents the possibility of early
			// exit of a workflow when a subworkflow whose ParentInstanceID == ""
			// completes.
			wfiMap := make(map[string]*core.WorkflowInstance)
			for _, workflowEvent := range result.WorkflowEvents {
				gotNewEvents = true
				instanceID := workflowEvent.WorkflowInstance.InstanceID
				if wfi, ok := wfiMap[instanceID]; ok {
					if wfi.ParentInstanceID != "" && workflowEvent.WorkflowInstance.ParentInstanceID == "" {
						workflowEvent.WorkflowInstance.ParentInstanceID = wfi.ParentInstanceID
						workflowEvent.WorkflowInstance.ParentEventID = wfi.ParentEventID
					} else if wfi.ParentInstanceID == "" && workflowEvent.WorkflowInstance.ParentInstanceID != "" {
						wfi.ParentInstanceID = workflowEvent.WorkflowInstance.ParentInstanceID
						wfi.ParentEventID = workflowEvent.WorkflowInstance.ParentEventID
					}
				} else {
					wfiMap[instanceID] = workflowEvent.WorkflowInstance
				}
			}

			for _, workflowEvent := range result.WorkflowEvents {
				gotNewEvents = true
				wt.logger.Debug("Workflow event", "event_type", workflowEvent.HistoryEvent.Type)

				switch workflowEvent.HistoryEvent.Type {
				case history.EventType_WorkflowExecutionStarted:
					wt.scheduleSubWorkflow(workflowEvent)

				default:
					wt.sendEvent(workflowEvent.WorkflowInstance, workflowEvent.HistoryEvent)
				}
			}

			for _, timerEvent := range result.TimerEvents {
				gotNewEvents = true
				wt.logger.Debug("Timer event", "event_type", timerEvent.Type)

				wt.scheduleTimer(tw.instance, timerEvent)
			}
		}

		for !wt.workflowFinished && !gotNewEvents {
			// No new events left and the workflow isn't finished yet. Check for timers or callbacks
			select {
			case callback := <-wt.callbacks:
				event := callback()
				if event != nil {
					wt.sendEvent(event.WorkflowInstance, event.HistoryEvent)
					gotNewEvents = true
				}
				continue
			default:
			}

			if len(wt.timers) > 0 {
				// Take first timer and execute it
				sort.SliceStable(wt.timers, func(i, j int) bool {
					return wt.timers[i].At.Before(wt.timers[j].At)
				})

				t := wt.timers[0]
				wt.timers = wt.timers[1:]

				// Advance workflow clock to fire the timer
				wt.logger.Debug("Advancing workflow clock to fire timer")
				wt.clock.Set(t.At)
				t.Callback()
			} else {
				t := time.NewTimer(wt.options.TestTimeout)

				select {
				case callback := <-wt.callbacks:
					event := callback()
					if event != nil {
						wt.sendEvent(event.WorkflowInstance, event.HistoryEvent)
						gotNewEvents = true
					}
				case <-t.C:
					t.Stop()
					panic("No new events generated during workflow execution and no pending timers, workflow blocked?")
				}
			}
		}
	}
}

func (wt *workflowTester[TResult]) sendEvent(wfi *core.WorkflowInstance, event history.Event) {
	var w *testWorkflow
	for _, tw := range wt.testWorkflows {
		if tw.instance.InstanceID == wfi.InstanceID {
			w = tw
			break
		}
	}

	if w == nil {
		// Workflow not mocked, create new instance
		w = &testWorkflow{
			instance:      wfi,
			history:       []history.Event{},
			pendingEvents: []history.Event{},
		}

		wt.testWorkflows = append(wt.testWorkflows, w)
	}

	w.pendingEvents = append(w.pendingEvents, event)
}

func (wt *workflowTester[TResult]) SignalWorkflow(name string, value interface{}) {
	wt.SignalWorkflowInstance(wt.wfi, name, value)
}

func (wt *workflowTester[TResult]) SignalWorkflowInstance(wfi *core.WorkflowInstance, name string, value interface{}) {
	arg, err := converter.DefaultConverter.To(value)
	if err != nil {
		panic("Could not convert signal value to string" + err.Error())
	}

	wt.callbacks <- func() *history.WorkflowEvent {
		e := history.NewPendingEvent(
			wt.clock.Now(),
			history.EventType_SignalReceived,
			&history.SignalReceivedAttributes{
				Name: name,
				Arg:  arg,
			},
		)

		return &history.WorkflowEvent{
			WorkflowInstance: wfi,
			HistoryEvent:     e,
		}
	}
}

func (wt *workflowTester[TResult]) WorkflowFinished() bool {
	return wt.workflowFinished
}

func (wt *workflowTester[TResult]) WorkflowResult() (TResult, string) {
	var r TResult
	if wt.workflowResult != nil {
		if err := converter.DefaultConverter.From(wt.workflowResult, &r); err != nil {
			panic("could not convert workflow result to expected type" + err.Error())
		}
	}

	return r, wt.workflowErr
}

func (wt *workflowTester[TResult]) AssertExpectations(t *testing.T) {
	wt.ma.AssertExpectations(t)
}

func (wt *workflowTester[TResult]) scheduleActivity(wfi *core.WorkflowInstance, event history.Event) {
	e := event.Attributes.(*history.ActivityScheduledAttributes)

	go func() {
		atomic.AddInt32(&wt.runningActivities, 1)
		defer atomic.AddInt32(&wt.runningActivities, -1)

		var activityErr error
		var activityResult payload.Payload

		// Execute mocked activity. If an activity is mocked once, we'll never fall back to the original implementation
		if wt.mockedActivities[e.Name] {
			afn, err := wt.registry.GetActivity(e.Name)
			if err != nil {
				panic("Could not find activity " + e.Name + " in registry")
			}

			argValues, addContext, err := margs.InputsToArgs(converter.DefaultConverter, reflect.ValueOf(afn), e.Inputs)
			if err != nil {
				panic("Could not convert activity inputs to args: " + err.Error())
			}

			args := make([]interface{}, len(argValues))
			for i, arg := range argValues {
				if i == 0 && addContext {
					args[i] = context.Background()
					continue
				}

				args[i] = arg.Interface()
			}

			results := wt.ma.MethodCalled(e.Name, args...)

			switch len(results) {
			case 1:
				// Expect only error
				activityErr = results.Error(0)
				activityResult = nil
			case 2:
				result := results.Get(0)
				activityResult, err = converter.DefaultConverter.To(result)
				if err != nil {
					panic("Could not convert result for activity " + e.Name + ": " + err.Error())
				}

				activityErr = results.Error(1)
			default:
				panic(
					fmt.Sprintf(
						"Unexpected number of results returned for mocked activity %v, expected 1 or 2, got %v",
						e.Name,
						len(results),
					),
				)
			}

		} else {
			executor := activity.NewExecutor(wt.logger, wt.tracer, wt.registry)
			activityResult, activityErr = executor.ExecuteActivity(context.Background(), &task.Activity{
				ID:               uuid.NewString(),
				Metadata:         &core.WorkflowMetadata{},
				WorkflowInstance: wfi,
				Event:            event,
			})
		}

		wt.callbacks <- func() *history.WorkflowEvent {
			var ne history.Event

			if activityErr != nil {
				ne = history.NewPendingEvent(
					wt.clock.Now(),
					history.EventType_ActivityFailed,
					&history.ActivityFailedAttributes{
						Reason: activityErr.Error(),
					},
					history.ScheduleEventID(event.ScheduleEventID),
				)
			} else {
				ne = history.NewPendingEvent(
					wt.clock.Now(),
					history.EventType_ActivityCompleted,
					&history.ActivityCompletedAttributes{
						Result: activityResult,
					},
					history.ScheduleEventID(event.ScheduleEventID),
				)
			}

			return &history.WorkflowEvent{
				WorkflowInstance: wfi,
				HistoryEvent:     ne,
			}
		}
	}()
}

func (wt *workflowTester[TResult]) scheduleTimer(instance *core.WorkflowInstance, event history.Event) {
	e := event.Attributes.(*history.TimerFiredAttributes)

	wt.timers = append(wt.timers, &testTimer{
		At: e.At,
		Callback: func() {
			wt.callbacks <- func() *history.WorkflowEvent {
				return &history.WorkflowEvent{
					WorkflowInstance: instance,
					HistoryEvent:     event,
				}
			}
		},
	})
}

func (wt *workflowTester[TResult]) scheduleSubWorkflow(event history.WorkflowEvent) {
	a := event.HistoryEvent.Attributes.(*history.ExecutionStartedAttributes)

	// TODO: Right location to call handler?
	if wt.subWorkflowListener != nil {
		wt.subWorkflowListener(event.WorkflowInstance, a.Name)
	}

	wfn, err := wt.registry.GetWorkflow(a.Name)
	if err != nil {
		panic("Could not find workflow " + a.Name + " in registry")
	}

	argValues, addContext, err := margs.InputsToArgs(converter.DefaultConverter, reflect.ValueOf(wfn), a.Inputs)
	if err != nil {
		panic("Could not convert workflow inputs to args: " + err.Error())
	}

	args := make([]interface{}, len(argValues))
	for i, arg := range argValues {
		if i == 0 && addContext {
			args[i] = context.Background()
			continue
		}

		args[i] = arg.Interface()
	}

	if !wt.mockedWorkflows[a.Name] {
		// Workflow not mocked, allow event to be processed
		wt.sendEvent(event.WorkflowInstance, event.HistoryEvent)
		return
	}

	var workflowErr error
	var workflowResult payload.Payload

	results := wt.mw.MethodCalled(a.Name, args...)

	switch len(results) {
	case 1:
		// Expect only error
		workflowErr = results.Error(0)
		workflowResult = nil
	case 2:
		result := results.Get(0)
		workflowResult, err = converter.DefaultConverter.To(result)
		if err != nil {
			panic("Could not convert result for mocked workflow " + a.Name + ": " + err.Error())
		}

		workflowErr = results.Error(1)
	default:
		panic(
			fmt.Sprintf(
				"Unexpected number of results returned for mocked workflow %v, expected 1 or 2, got %v",
				a.Name,
				len(results),
			),
		)
	}

	wt.callbacks <- func() *history.WorkflowEvent {
		r := command.NewCompleteWorkflowCommand(0, event.WorkflowInstance, workflowResult, workflowErr).Execute(wt.clock)

		return &r.WorkflowEvents[0]
	}
}

func (wt *workflowTester[TResult]) getInitialEvent(wf interface{}, args []interface{}) history.Event {
	name := fn.Name(wf)

	inputs, err := margs.ArgsToInputs(converter.DefaultConverter, args...)
	if err != nil {
		panic(err)
	}

	return history.NewHistoryEvent(
		1,
		wt.clock.Now(),
		history.EventType_WorkflowExecutionStarted,
		&history.ExecutionStartedAttributes{
			Name:     name,
			Metadata: &core.WorkflowMetadata{},
			Inputs:   inputs,
		},
	)
}

func getNextWorkflowTask(wfi *core.WorkflowInstance, history []history.Event, newEvents []history.Event) *task.Workflow {
	var lastSequenceID int64
	if len(history) > 0 {
		lastSequenceID = history[len(history)-1].SequenceID
	}

	return &task.Workflow{
		WorkflowInstance: wfi,
		Metadata:         &core.WorkflowMetadata{},
		LastSequenceID:   lastSequenceID,
		NewEvents:        newEvents,
	}
}
