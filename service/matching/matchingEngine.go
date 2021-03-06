// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package matching

import (
	"context"
	"errors"
	"math"
	"sync"

	"github.com/pborman/uuid"
	"github.com/uber-common/bark"
	h "github.com/uber/cadence/.gen/go/history"
	m "github.com/uber/cadence/.gen/go/matching"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/client/history"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/client"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
)

// Implements matching.Engine
// TODO: Switch implementation from lock/channel based to a partitioned agent
// to simplify code and reduce possiblity of synchronization errors.
type matchingEngineImpl struct {
	taskManager     persistence.TaskManager
	historyService  history.Client
	tokenSerializer common.TaskTokenSerializer
	logger          bark.Logger
	metricsClient   metrics.Client
	taskListsLock   sync.RWMutex                   // locks mutation of taskLists
	taskLists       map[taskListID]taskListManager // Convert to LRU cache
	config          *Config
	queryMapLock    sync.Mutex
	// map from query TaskID (which is a UUID generated in QueryWorkflow() call) to a channel that QueryWorkflow()
	// will block and wait for. The RespondQueryTaskCompleted() call will send the data through that channel which will
	// unblock QueryWorkflow() call.
	queryTaskMap map[string]chan *workflow.RespondQueryTaskCompletedRequest
}

type taskListID struct {
	domainID     string
	taskListName string
	taskType     int
}

type pollerIDCtxKey string
type identityCtxKey string

var (
	// EmptyPollForDecisionTaskResponse is the response when there are no decision tasks to hand out
	emptyPollForDecisionTaskResponse = &m.PollForDecisionTaskResponse{}
	// EmptyPollForActivityTaskResponse is the response when there are no activity tasks to hand out
	emptyPollForActivityTaskResponse   = &workflow.PollForActivityTaskResponse{}
	persistenceOperationRetryPolicy    = common.CreatePersistanceRetryPolicy()
	historyServiceOperationRetryPolicy = common.CreateHistoryServiceRetryPolicy()

	// ErrNoTasks is exported temporarily for integration test
	ErrNoTasks    = errors.New("No tasks")
	errPumpClosed = errors.New("Task list pump closed its channel")

	pollerIDKey pollerIDCtxKey = "pollerID"
	identityKey identityCtxKey = "identity"
)

func (t *taskListID) String() string {
	var r string
	if t.taskType == persistence.TaskListTypeActivity {
		r += "activity"
	} else {
		r += "decision"
	}
	r += " task list \""
	r += t.taskListName
	r += "\""
	return r
}

var _ Engine = (*matchingEngineImpl)(nil) // Asserts that interface is indeed implemented

// NewEngine creates an instance of matching engine
func NewEngine(taskManager persistence.TaskManager,
	historyService history.Client,
	config *Config,
	logger bark.Logger,
	metricsClient metrics.Client) Engine {

	return &matchingEngineImpl{
		taskManager:     taskManager,
		historyService:  historyService,
		tokenSerializer: common.NewJSONTaskTokenSerializer(),
		taskLists:       make(map[taskListID]taskListManager),
		logger: logger.WithFields(bark.Fields{
			logging.TagWorkflowComponent: logging.TagValueMatchingEngineComponent,
		}),
		metricsClient: metricsClient,
		config:        config,
		queryTaskMap:  make(map[string]chan *workflow.RespondQueryTaskCompletedRequest),
	}
}

func (e *matchingEngineImpl) Start() {
	// As task lists are initialized lazily nothing is done on startup at this point.
}

func (e *matchingEngineImpl) Stop() {
	// Executes Stop() on each task list outside of lock
	for _, l := range e.getTaskLists(math.MaxInt32) {
		l.Stop()
	}
}

func (e *matchingEngineImpl) getTaskLists(maxCount int) (lists []taskListManager) {
	e.taskListsLock.RLock()
	defer e.taskListsLock.RUnlock()
	lists = make([]taskListManager, 0, len(e.taskLists))
	count := 0
	for _, tlMgr := range e.taskLists {
		lists = append(lists, tlMgr)
		count++
		if count >= maxCount {
			break
		}
	}
	return
}

func (e *matchingEngineImpl) String() string {
	// Executes taskList.String() on each task list outside of lock
	var r string
	for _, l := range e.getTaskLists(1000) {
		r += "\n"
		r += l.String()
	}
	return r
}

// Returns taskListManager for a task list. If not already cached gets new range from DB and
// if successful creates one.
func (e *matchingEngineImpl) getTaskListManager(taskList *taskListID,
	taskListKind *workflow.TaskListKind) (taskListManager, error) {
	// The first check is an optimization so almost all requests will have a task list manager
	// and return avoiding the write lock
	e.taskListsLock.RLock()
	if result, ok := e.taskLists[*taskList]; ok {
		e.taskListsLock.RUnlock()
		return result, nil
	}
	e.taskListsLock.RUnlock()
	// If it gets here, write lock and check again in case a task list is created between the two locks
	e.taskListsLock.Lock()
	if result, ok := e.taskLists[*taskList]; ok {
		e.taskListsLock.Unlock()
		return result, nil
	}
	mgr := newTaskListManager(e, taskList, taskListKind, e.config)
	e.taskLists[*taskList] = mgr
	e.taskListsLock.Unlock()
	logging.LogTaskListLoadingEvent(e.logger, taskList.taskListName, taskList.taskType)
	err := mgr.Start()
	if err != nil {
		logging.LogTaskListLoadingFailedEvent(e.logger, taskList.taskListName, taskList.taskType, err)
		return nil, err
	}
	logging.LogTaskListLoadedEvent(e.logger, taskList.taskListName, taskList.taskType)
	return mgr, nil
}

// For use in tests
func (e *matchingEngineImpl) updateTaskList(taskList *taskListID, mgr taskListManager) {
	e.taskListsLock.Lock()
	defer e.taskListsLock.Unlock()
	e.taskLists[*taskList] = mgr
}

func (e *matchingEngineImpl) removeTaskListManager(id *taskListID) {
	e.taskListsLock.Lock()
	defer e.taskListsLock.Unlock()
	delete(e.taskLists, *id)
}

// AddDecisionTask either delivers task directly to waiting poller or save it into task list persistence.
func (e *matchingEngineImpl) AddDecisionTask(addRequest *m.AddDecisionTaskRequest) error {
	domainID := addRequest.GetDomainUUID()
	taskListName := addRequest.TaskList.GetName()
	taskListKind := common.TaskListKindPtr(addRequest.TaskList.GetKind())
	e.logger.Debugf("Received AddDecisionTask for taskList=%v, WorkflowID=%v, RunID=%v, ScheduleToStartTimeout=%v",
		addRequest.TaskList.GetName(), addRequest.Execution.GetWorkflowId(), addRequest.Execution.GetRunId(),
		addRequest.GetScheduleToStartTimeoutSeconds())
	taskList := newTaskListID(domainID, taskListName, persistence.TaskListTypeDecision)
	tlMgr, err := e.getTaskListManager(taskList, taskListKind)
	if err != nil {
		return err
	}
	taskInfo := &persistence.TaskInfo{
		DomainID:               domainID,
		RunID:                  addRequest.Execution.GetRunId(),
		WorkflowID:             addRequest.Execution.GetWorkflowId(),
		ScheduleID:             addRequest.GetScheduleId(),
		ScheduleToStartTimeout: addRequest.GetScheduleToStartTimeoutSeconds(),
	}
	return tlMgr.AddTask(addRequest.Execution, taskInfo)
}

// AddActivityTask either delivers task directly to waiting poller or save it into task list persistence.
func (e *matchingEngineImpl) AddActivityTask(addRequest *m.AddActivityTaskRequest) error {
	domainID := addRequest.GetDomainUUID()
	sourceDomainID := addRequest.GetSourceDomainUUID()
	taskListName := addRequest.TaskList.GetName()
	taskListKind := common.TaskListKindPtr(addRequest.TaskList.GetKind())
	e.logger.Debugf("Received AddActivityTask for taskList=%v WorkflowID=%v, RunID=%v",
		taskListName, addRequest.Execution.WorkflowId, addRequest.Execution.RunId)
	taskList := newTaskListID(domainID, taskListName, persistence.TaskListTypeActivity)
	tlMgr, err := e.getTaskListManager(taskList, taskListKind)
	if err != nil {
		return err
	}
	taskInfo := &persistence.TaskInfo{
		DomainID:               sourceDomainID,
		RunID:                  addRequest.Execution.GetRunId(),
		WorkflowID:             addRequest.Execution.GetWorkflowId(),
		ScheduleID:             addRequest.GetScheduleId(),
		ScheduleToStartTimeout: addRequest.GetScheduleToStartTimeoutSeconds(),
	}
	return tlMgr.AddTask(addRequest.Execution, taskInfo)
}

// PollForDecisionTask tries to get the decision task using exponential backoff.
func (e *matchingEngineImpl) PollForDecisionTask(ctx context.Context, req *m.PollForDecisionTaskRequest) (
	*m.PollForDecisionTaskResponse, error) {
	domainID := req.GetDomainUUID()
	pollerID := req.GetPollerID()
	request := req.PollRequest
	taskListName := request.TaskList.GetName()
	e.logger.Debugf("Received PollForDecisionTask for taskList=%v", taskListName)
pollLoop:
	for {
		err := common.IsValidContext(ctx)
		if err != nil {
			return nil, err
		}
		// Add frontend generated pollerID to context so tasklistMgr can support cancellation of
		// long-poll when frontend calls CancelOutstandingPoll API
		pollerCtx := context.WithValue(ctx, pollerIDKey, pollerID)
		pollerCtx = context.WithValue(pollerCtx, identityKey, request.GetIdentity())
		taskList := newTaskListID(domainID, taskListName, persistence.TaskListTypeDecision)
		taskListKind := common.TaskListKindPtr(request.TaskList.GetKind())
		tCtx, err := e.getTask(pollerCtx, taskList, nil, taskListKind)
		if err != nil {
			// TODO: Is empty poll the best reply for errPumpClosed?
			if err == ErrNoTasks || err == errPumpClosed {
				return emptyPollForDecisionTaskResponse, nil
			}
			return nil, err
		}

		if tCtx.queryTaskInfo != nil {
			// for query task, we don't need to update history to record decision task started. but we need to know
			// the NextEventID so front end knows what are the history events to load for this decision task.
			mutableStateResp, err := e.historyService.GetMutableState(ctx, &h.GetMutableStateRequest{
				DomainUUID: req.DomainUUID,
				Execution:  &tCtx.workflowExecution,
			})
			if err != nil {
				// will notify query client that the query task failed
				completeType := workflow.QueryTaskCompletedTypeFailed
				e.RespondQueryTaskCompleted(ctx, &m.RespondQueryTaskCompletedRequest{
					TaskID: common.StringPtr(tCtx.queryTaskInfo.taskID),
					CompletedRequest: &workflow.RespondQueryTaskCompletedRequest{
						CompletedType: &completeType,
						ErrorMessage:  common.StringPtr("server internal error: failed to get nextID " + err.Error()),
					},
				})
				return emptyPollForDecisionTaskResponse, nil
			}

			clientFeature := client.NewFeatureImpl(
				mutableStateResp.GetClientLibraryVersion(),
				mutableStateResp.GetClientFeatureVersion(),
				mutableStateResp.GetClientImpl(),
			)

			isStickyEnabled := false
			if len(mutableStateResp.StickyTaskList.GetName()) != 0 && clientFeature.SupportStickyQuery() {
				isStickyEnabled = true
			}
			resp := &h.RecordDecisionTaskStartedResponse{
				PreviousStartedEventId: mutableStateResp.NextEventId,
				NextEventId:            mutableStateResp.NextEventId,
				WorkflowType:           mutableStateResp.WorkflowType,
				StickyExecutionEnabled: common.BoolPtr(isStickyEnabled),
			}
			tCtx.completeTask(nil)
			return e.createPollForDecisionTaskResponse(tCtx, resp), nil
		}

		// Generate a unique requestId for this task which will be used for all retries
		requestID := uuid.New()
		resp, err := tCtx.RecordDecisionTaskStartedWithRetry(&h.RecordDecisionTaskStartedRequest{
			DomainUUID:        common.StringPtr(domainID),
			WorkflowExecution: &tCtx.workflowExecution,
			ScheduleId:        &tCtx.info.ScheduleID,
			TaskId:            &tCtx.info.TaskID,
			RequestId:         common.StringPtr(requestID),
			PollRequest:       request,
		})
		if err != nil {
			switch err.(type) {
			case *workflow.EntityNotExistsError, *h.EventAlreadyStartedError:
				e.logger.Debugf("Duplicated decision task taskList=%v, taskID=%v",
					taskListName, tCtx.info.TaskID)
				tCtx.completeTask(nil)
			default:
				tCtx.completeTask(err)
			}

			continue pollLoop
		}
		tCtx.completeTask(nil)
		return e.createPollForDecisionTaskResponse(tCtx, resp), nil
	}
}

// pollForActivityTaskOperation takes one task from the task manager, update workflow execution history, mark task as
// completed and return it to user. If a task from task manager is already started, return an empty response, without
// error. Timeouts handled by the timer queue.
func (e *matchingEngineImpl) PollForActivityTask(ctx context.Context, req *m.PollForActivityTaskRequest) (
	*workflow.PollForActivityTaskResponse, error) {
	domainID := req.GetDomainUUID()
	pollerID := req.GetPollerID()
	request := req.PollRequest
	taskListName := request.TaskList.GetName()
	e.logger.Debugf("Received PollForActivityTask for taskList=%v", taskListName)
pollLoop:
	for {
		err := common.IsValidContext(ctx)
		if err != nil {
			return nil, err
		}

		taskList := newTaskListID(domainID, taskListName, persistence.TaskListTypeActivity)
		var maxDispatch *float64
		if request.TaskListMetadata != nil {
			maxDispatch = request.TaskListMetadata.MaxTasksPerSecond
		}
		// Add frontend generated pollerID to context so tasklistMgr can support cancellation of
		// long-poll when frontend calls CancelOutstandingPoll API
		pollerCtx := context.WithValue(ctx, pollerIDKey, pollerID)
		pollerCtx = context.WithValue(pollerCtx, identityKey, request.GetIdentity())
		taskListKind := common.TaskListKindPtr(request.TaskList.GetKind())
		tCtx, err := e.getTask(pollerCtx, taskList, maxDispatch, taskListKind)
		if err != nil {
			// TODO: Is empty poll the best reply for errPumpClosed?
			if err == ErrNoTasks || err == errPumpClosed {
				return emptyPollForActivityTaskResponse, nil
			}
			return nil, err
		}
		// Generate a unique requestId for this task which will be used for all retries
		requestID := uuid.New()
		resp, err := tCtx.RecordActivityTaskStartedWithRetry(&h.RecordActivityTaskStartedRequest{
			DomainUUID:        common.StringPtr(domainID),
			WorkflowExecution: &tCtx.workflowExecution,
			ScheduleId:        &tCtx.info.ScheduleID,
			TaskId:            &tCtx.info.TaskID,
			RequestId:         common.StringPtr(requestID),
			PollRequest:       request,
		})
		if err != nil {
			switch err.(type) {
			case *workflow.EntityNotExistsError, *h.EventAlreadyStartedError:
				e.logger.Debugf("Duplicated activity task taskList=%v, taskID=%v",
					taskListName, tCtx.info.TaskID)
				tCtx.completeTask(nil)
			default:
				tCtx.completeTask(err)
			}

			continue pollLoop
		}
		tCtx.completeTask(nil)
		return e.createPollForActivityTaskResponse(tCtx, resp), nil
	}
}

// QueryWorkflow creates a DecisionTask with query data, send it through sync match channel, wait for that DecisionTask
// to be processed by worker, and then return the query result.
func (e *matchingEngineImpl) QueryWorkflow(ctx context.Context, queryRequest *m.QueryWorkflowRequest) (*workflow.QueryWorkflowResponse, error) {
	domainID := queryRequest.GetDomainUUID()
	taskListName := queryRequest.TaskList.GetName()
	taskList := newTaskListID(domainID, taskListName, persistence.TaskListTypeDecision)
	taskListKind := common.TaskListKindPtr(queryRequest.TaskList.GetKind())
	tlMgr, err := e.getTaskListManager(taskList, taskListKind)
	if err != nil {
		return nil, err
	}
	queryTask := &queryTaskInfo{
		queryRequest: queryRequest,
		taskID:       uuid.New(),
	}
	err = tlMgr.SyncMatchQueryTask(ctx, queryTask)
	if err != nil {
		return nil, err
	}

	queryResultCh := make(chan *workflow.RespondQueryTaskCompletedRequest, 1)
	e.queryMapLock.Lock()
	e.queryTaskMap[queryTask.taskID] = queryResultCh
	e.queryMapLock.Unlock()
	defer func() {
		e.queryMapLock.Lock()
		delete(e.queryTaskMap, queryTask.taskID)
		e.queryMapLock.Unlock()
	}()

	select {
	case result := <-queryResultCh:
		if *result.CompletedType == workflow.QueryTaskCompletedTypeFailed {
			return nil, &workflow.QueryFailedError{Message: result.GetErrorMessage()}
		}
		return &workflow.QueryWorkflowResponse{QueryResult: result.QueryResult}, nil
	case <-ctx.Done():
		return nil, &workflow.QueryFailedError{Message: "timeout: workflow worker is not responding"}
	}
}

func (e *matchingEngineImpl) RespondQueryTaskCompleted(ctx context.Context, request *m.RespondQueryTaskCompletedRequest) error {
	e.queryMapLock.Lock()
	queryResultCh, ok := e.queryTaskMap[request.GetTaskID()]
	e.queryMapLock.Unlock()
	if !ok {
		e.metricsClient.IncCounter(metrics.MatchingRespondQueryTaskCompletedScope, metrics.RespondQueryTaskFailedCounter)
		return &workflow.EntityNotExistsError{Message: "query task not found, or already expired"}
	}

	queryResultCh <- request.CompletedRequest

	return nil
}

func (e *matchingEngineImpl) CancelOutstandingPoll(ctx context.Context, request *m.CancelOutstandingPollRequest) error {
	domainID := request.GetDomainUUID()
	taskListType := int(request.GetTaskListType())
	taskListName := request.TaskList.GetName()
	pollerID := request.GetPollerID()

	taskList := newTaskListID(domainID, taskListName, taskListType)
	taskListKind := common.TaskListKindPtr(request.TaskList.GetKind())
	tlMgr, err := e.getTaskListManager(taskList, taskListKind)
	if err != nil {
		return err
	}

	tlMgr.CancelPoller(pollerID)
	return nil
}

func (e *matchingEngineImpl) DescribeTaskList(ctx context.Context, request *m.DescribeTaskListRequest) (*workflow.DescribeTaskListResponse, error) {
	domainID := request.GetDomainUUID()
	taskListType := persistence.TaskListTypeDecision
	if request.DescRequest.GetTaskListType() == workflow.TaskListTypeActivity {
		taskListType = persistence.TaskListTypeActivity
	}
	taskListName := request.DescRequest.TaskList.GetName()

	taskList := newTaskListID(domainID, taskListName, taskListType)
	taskListKind := common.TaskListKindPtr(request.DescRequest.TaskList.GetKind())
	tlMgr, err := e.getTaskListManager(taskList, taskListKind)
	if err != nil {
		return nil, err
	}

	pollers := []*workflow.PollerInfo{}
	for _, poller := range tlMgr.GetAllPollerInfo() {
		pollers = append(pollers, &workflow.PollerInfo{
			Identity:       common.StringPtr(poller.identity),
			LastAccessTime: common.Int64Ptr(poller.lastAccessTime.UnixNano()),
		})
	}
	return &workflow.DescribeTaskListResponse{Pollers: pollers}, nil
}

// Loads a task from persistence and wraps it in a task context
func (e *matchingEngineImpl) getTask(
	ctx context.Context, taskList *taskListID, maxDispatchPerSecond *float64, taskListKind *workflow.TaskListKind,
) (*taskContext, error) {
	tlMgr, err := e.getTaskListManager(taskList, taskListKind)
	if err != nil {
		return nil, err
	}
	return tlMgr.GetTaskContext(ctx, maxDispatchPerSecond)
}

func (e *matchingEngineImpl) unloadTaskList(id *taskListID) {
	e.taskListsLock.Lock()
	tlMgr, ok := e.taskLists[*id]
	if ok {
		delete(e.taskLists, *id)
	}
	e.taskListsLock.Unlock()
	if ok {
		tlMgr.Stop()
	}
}

// Populate the decision task response based on context and scheduled/started events.
func (e *matchingEngineImpl) createPollForDecisionTaskResponse(context *taskContext,
	historyResponse *h.RecordDecisionTaskStartedResponse) *m.PollForDecisionTaskResponse {
	task := context.info

	response := &m.PollForDecisionTaskResponse{}
	response.WorkflowExecution = workflowExecutionPtr(context.workflowExecution)
	if context.queryTaskInfo != nil {
		// for a query task
		queryRequest := context.queryTaskInfo.queryRequest
		token := &common.QueryTaskToken{
			DomainID: *queryRequest.DomainUUID,
			TaskList: *queryRequest.TaskList.Name,
			TaskID:   context.queryTaskInfo.taskID,
		}
		response.TaskToken, _ = e.tokenSerializer.SerializeQueryTaskToken(token)
		response.Query = context.queryTaskInfo.queryRequest.QueryRequest.Query
	} else {
		token := &common.TaskToken{
			DomainID:        task.DomainID,
			WorkflowID:      task.WorkflowID,
			RunID:           task.RunID,
			ScheduleID:      historyResponse.GetScheduledEventId(),
			ScheduleAttempt: historyResponse.GetAttempt(),
		}
		response.TaskToken, _ = e.tokenSerializer.Serialize(token)
		response.WorkflowType = historyResponse.WorkflowType
		response.Attempt = common.Int64Ptr(historyResponse.GetAttempt())
	}
	if historyResponse.GetPreviousStartedEventId() != common.EmptyEventID {
		response.PreviousStartedEventId = historyResponse.PreviousStartedEventId
	}
	response.WorkflowType = historyResponse.WorkflowType
	response.StartedEventId = historyResponse.StartedEventId
	response.StickyExecutionEnabled = historyResponse.StickyExecutionEnabled
	response.BacklogCountHint = common.Int64Ptr(context.backlogCountHint)
	response.NextEventId = historyResponse.NextEventId
	response.DecisionInfo = historyResponse.DecisionInfo

	return response
}

// Populate the activity task response based on context and scheduled/started events.
func (e *matchingEngineImpl) createPollForActivityTaskResponse(context *taskContext,
	historyResponse *h.RecordActivityTaskStartedResponse) *workflow.PollForActivityTaskResponse {
	task := context.info

	startedEvent := historyResponse.StartedEvent
	scheduledEvent := historyResponse.ScheduledEvent
	if scheduledEvent.ActivityTaskScheduledEventAttributes == nil {
		panic("GetActivityTaskScheduledEventAttributes is not set")
	}
	attributes := scheduledEvent.ActivityTaskScheduledEventAttributes
	if attributes.ActivityId == nil {
		panic("ActivityTaskScheduledEventAttributes.ActivityID is not set")
	}

	response := &workflow.PollForActivityTaskResponse{}
	response.ActivityId = attributes.ActivityId
	response.ActivityType = attributes.ActivityType
	response.Input = attributes.Input
	response.WorkflowExecution = workflowExecutionPtr(context.workflowExecution)
	response.ScheduledTimestamp = common.Int64Ptr(*scheduledEvent.Timestamp)
	response.ScheduleToCloseTimeoutSeconds = common.Int32Ptr(*attributes.ScheduleToCloseTimeoutSeconds)
	response.StartedTimestamp = common.Int64Ptr(*startedEvent.Timestamp)
	response.StartToCloseTimeoutSeconds = common.Int32Ptr(*attributes.StartToCloseTimeoutSeconds)
	response.HeartbeatTimeoutSeconds = common.Int32Ptr(*attributes.HeartbeatTimeoutSeconds)

	token := &common.TaskToken{
		DomainID:   task.DomainID,
		WorkflowID: task.WorkflowID,
		RunID:      task.RunID,
		ScheduleID: task.ScheduleID,
	}
	response.TaskToken, _ = e.tokenSerializer.Serialize(token)
	return response
}

func newTaskListID(domainID, taskListName string, taskType int) *taskListID {
	return &taskListID{domainID: domainID, taskListName: taskListName, taskType: taskType}
}

func workflowExecutionPtr(execution workflow.WorkflowExecution) *workflow.WorkflowExecution {
	return &execution
}
