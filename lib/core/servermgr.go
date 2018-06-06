package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/MG-RAST/AWE/lib/conf"
	"github.com/MG-RAST/AWE/lib/core/cwl"
	e "github.com/MG-RAST/AWE/lib/errors"
	"github.com/MG-RAST/AWE/lib/logger"
	"github.com/MG-RAST/AWE/lib/logger/event"
	"github.com/MG-RAST/AWE/lib/shock"
	"github.com/MG-RAST/AWE/lib/user"
	"github.com/davecgh/go-spew/spew"
	"github.com/robertkrimen/otto"
	"gopkg.in/mgo.v2/bson"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type jQueueShow struct {
	Active  map[string]*JobPerf `bson:"active" json:"active"`
	Suspend map[string]bool     `bson:"suspend" json:"suspend"`
}

type ServerMgr struct {
	CQMgr
	queueLock      sync.Mutex //only update one at a time
	lastUpdate     time.Time
	lastUpdateLock sync.RWMutex
	TaskMap        TaskMap
	taskIn         chan *Task //channel for receiving Task (JobController -> qmgr.Handler)
	ajLock         sync.RWMutex
	//sjLock         sync.RWMutex
	actJobs map[string]*JobPerf
	//susJobs        map[string]bool
}

func NewServerMgr() *ServerMgr {
	return &ServerMgr{
		CQMgr: CQMgr{
			clientMap:    *NewClientMap(),
			workQueue:    NewWorkQueue(),
			suspendQueue: false,

			coReq:    make(chan CoReq, conf.COREQ_LENGTH), // number of clients that wait in queue to get a workunit. If queue is full, other client will be rejected and have to come back later again
			feedback: make(chan Notice),
			coSem:    make(chan int, 1), //non-blocking buffered channel

		},
		lastUpdate: time.Now().Add(time.Second * -30),
		TaskMap:    *NewTaskMap(),
		taskIn:     make(chan *Task, 1024),
		actJobs:    map[string]*JobPerf{},
		//susJobs:    map[string]bool{},
	}
}

//--------mgr methods-------

func (qm *ServerMgr) Lock()    {}
func (qm *ServerMgr) Unlock()  {}
func (qm *ServerMgr) RLock()   {}
func (qm *ServerMgr) RUnlock() {}

func (qm *ServerMgr) TaskHandle() { // TODO DEPRECATED
	logger.Info("TaskHandle is starting")
	for {
		task := <-qm.taskIn

		task_id, err := task.GetId("TaskHandle")
		if err != nil {
			logger.Error("(TaskHandle) %s", err.Error())
			continue
		}

		logger.Debug(2, "(ServerMgr/TaskHandle) received task from channel taskIn, id=%s", task_id)
		err = qm.addTask(task, nil)
		if err != nil {
			logger.Error("(ServerMgr/TaskHandle) qm.addTask failed: %s", err.Error())
		}
	}
}

func (qm *ServerMgr) UpdateQueueLoop() {
	// TODO this may not be dynamic enough for small amounts of workunits, as they always have to wait
	for {
		qm.updateQueue()
		time.Sleep(30 * time.Second)
	}
}

func (qm *ServerMgr) ClientHandle() {
	logger.Info("(ServerMgr ClientHandle) starting")
	count := 0

	time.Sleep(3 * time.Second)

	for {
		//select {
		//case coReq := <-qm.coReq
		//logger.Debug(3, "(ServerMgr ClientHandle) try to pull work request")
		//coReq, err := qm.requestQueue.Pull()
		//for err != nil {
		//	time.Sleep(50 * time.Millisecond) // give clients time to put in requests or get a response
		//	time.Sleep(3 * time.Second)
		//	coReq, err = qm.requestQueue.Pull()
		//	logger.Debug(3, "(ServerMgr ClientHandle) waiting")
		//}
		//logger.Debug(3, "(ServerMgr ClientHandle) got work request")

		coReq := <-qm.coReq //written to in cqmgr.go
		count += 1
		request_start_time := time.Now()
		logger.Debug(3, "(ServerMgr ClientHandle) workunit checkout request received from client %s, Req=%v", coReq.fromclient, coReq)

		ok, err := qm.CQMgr.clientMap.Has(coReq.fromclient, true)
		if err != nil {
			logger.Warning("(ServerMgr ClientHandle) Could not get lock for client %s (%s)", coReq.fromclient, err.Error())
			continue
		}
		if !ok {
			logger.Error("(ServerMgr ClientHandle) Client %s not found. (It probably left in the mean-time)", coReq.fromclient)
			continue
		}

		var ack CoAck
		if qm.suspendQueue {
			// queue is suspended, return suspend error
			ack = CoAck{workunits: nil, err: errors.New(e.QueueSuspend)}
			logger.Debug(3, "(ServerMgr ClientHandle %s) nowworkunit: e.QueueSuspend", coReq.fromclient)
		} else {
			logger.Debug(3, "(ServerMgr ClientHandle %s) popWorks", coReq.fromclient)

			works, err := qm.popWorks(coReq)
			if err != nil {
				logger.Debug(3, "(ServerMgr ClientHandle) popWorks returned error: %s", err.Error())
			}
			logger.Debug(3, "(ServerMgr ClientHandle %s) popWorks done", coReq.fromclient)
			if err == nil {
				logger.Debug(3, "(ServerMgr ClientHandle %s) UpdateJobTaskToInProgress", coReq.fromclient)

				qm.UpdateJobTaskToInProgress(works)

				logger.Debug(3, "(ServerMgr ClientHandle %s) UpdateJobTaskToInProgress done", coReq.fromclient)
			}
			ack = CoAck{workunits: works, err: err}

			if len(works) > 0 {
				wu := works[0]

				logger.Debug(3, "(ServerMgr ClientHandle %s) workunit: %s", coReq.fromclient, wu.Id)
			} else {
				logger.Debug(3, "(ServerMgr ClientHandle %s) works is empty", coReq.fromclient)
			}
		}
		logger.Debug(3, "(ServerMgr ClientHandle %s) send response now", coReq.fromclient)

		start_time := time.Now()

		timer := time.NewTimer(20 * time.Second)

		select {
		case coReq.response <- ack:
			logger.Debug(3, "(ServerMgr ClientHandle %s) send response (maybe workunit) to client via response channel", coReq.fromclient)
		case <-timer.C:
			elapsed_time := time.Since(start_time)
			logger.Error("(ServerMgr ClientHandle %s) timed out after %s ", coReq.fromclient, elapsed_time)
			continue
		}
		logger.Debug(3, "(ServerMgr ClientHandle %s) done", coReq.fromclient)

		if count%100 == 0 { // use modulo to reduce number of log messages
			request_time_elapsed := time.Since(request_start_time)

			logger.Info("(ServerMgr ClientHandle) Responding to work request took %s", request_time_elapsed)
		}
	}
}

func (qm *ServerMgr) NoticeHandle() {
	logger.Info("(ServerMgr NoticeHandle) starting")
	for {
		notice := <-qm.feedback

		id, err := notice.Id.String()
		if err != nil {
			logger.Error("(NoticeHandle) notice.Id invalid: " + err.Error())
			err = nil
			continue
		}

		logger.Debug(3, "(ServerMgr NoticeHandle) got notice: workid=%s, status=%s, clientid=%s", id, notice.Status, notice.WorkerId)
		err = qm.handleNoticeWorkDelivered(notice)
		if err != nil {
			err = fmt.Errorf("(NoticeHandle): %s", err.Error())
			logger.Error(err.Error())
			//fmt.Println(err.Error())
		}
	}
}

//--------queue status methods-------

func (qm *ServerMgr) SuspendQueue() {
	qm.suspendQueue = true
}

func (qm *ServerMgr) ResumeQueue() {
	qm.suspendQueue = false
}

func (qm *ServerMgr) QueueStatus() string {
	if qm.suspendQueue {
		return "suspended"
	} else {
		return "running"
	}
}

func (qm *ServerMgr) GetQueue(name string) interface{} {
	if name == "job" {
		suspended_jobs := qm.GetSuspendJobs()
		return jQueueShow{qm.actJobs, suspended_jobs}
	}
	if name == "task" {
		tasks, err := qm.TaskMap.GetTasks()
		if err != nil {
			return err
		}
		return tasks
	}
	if name == "workall" {
		workunits, err := qm.workQueue.all.GetWorkunits()
		if err != nil {
			return err
		}
		return workunits
	}
	if name == "workqueue" {
		workunits, err := qm.workQueue.Queue.GetWorkunits()
		if err != nil {
			return err
		}
		return workunits
	}
	if name == "workcheckout" {
		workunits, err := qm.workQueue.Checkout.GetWorkunits()
		if err != nil {
			return err
		}
		return workunits
	}
	if name == "worksuspend" {
		workunits, err := qm.workQueue.Suspend.GetWorkunits()
		if err != nil {
			return err
		}
		return workunits
	}
	if name == "client" {
		return qm.clientMap
	}
	return nil
}

//--------suspend job accessor methods-------

func (qm *ServerMgr) lenSusJobs() (l int) {

	l = 0
	jobs, _ := JM.Get_List(true) // TODO error handling

	for i := range jobs {
		job := jobs[i]
		state, _ := job.GetState(true)
		if state == JOB_STAT_SUSPEND {
			l += 1
		}

	}

	//qm.sjLock.RLock()
	//l = len(qm.susJobs)
	//qm.sjLock.RUnlock()
	return
}

//func (qm *ServerMgr) putSusJob(id string) {
//	qm.sjLock.Lock()
//	qm.susJobs[id] = true
//	qm.sjLock.Unlock()
//}

func (qm *ServerMgr) GetSuspendJobs() (sjobs map[string]bool) {

	jobs, _ := JM.Get_List(true) // TODO error handling

	sjobs = make(map[string]bool)

	for i := range jobs {
		job := jobs[i]
		state, _ := job.GetState(true) // TODO error handling
		if state == JOB_STAT_SUSPEND {
			id, _ := job.GetId(true)
			sjobs[id] = true
		}

	}

	// qm.sjLock.RLock()
	// 	defer qm.sjLock.RUnlock()
	// 	sjobs = make(map[string]bool)
	// 	for id, _ := range qm.susJobs {
	// 		sjobs[id] = true
	// 	}
	return
}

//func (qm *ServerMgr) removeSusJob(id string) {
//	qm.sjLock.Lock()
//	delete(qm.susJobs, id)
//	qm.sjLock.Unlock()
//}

func (qm *ServerMgr) isSusJob(id string) (has bool) {

	job, err := GetJob(id)
	if err != nil {
		return
	}

	job_state, err := job.GetState(true)
	if err != nil {
		return
	}

	has = false
	if job_state == JOB_STAT_COMPLETED {
		has = true
	}
	return
	//	qm.sjLock.RLock()
	//	defer qm.sjLock.RUnlock()
	//	if _, ok := qm.susJobs[id]; ok {
	//		has = true
	//	} else {
	//		has = false
	//	}
	//	return
}

//--------active job accessor methods-------

func (qm *ServerMgr) copyJobPerf(a *JobPerf) (b *JobPerf) {
	b = new(JobPerf)
	*b = *a
	return
}

func (qm *ServerMgr) lenActJobs() (l int) {
	qm.ajLock.RLock()
	l = len(qm.actJobs)
	qm.ajLock.RUnlock()
	return
}

func (qm *ServerMgr) putActJob(jperf *JobPerf) {
	qm.ajLock.Lock()
	qm.actJobs[jperf.Id] = jperf
	qm.ajLock.Unlock()
}

func (qm *ServerMgr) getActJob(id string) (*JobPerf, bool) {
	qm.ajLock.RLock()
	defer qm.ajLock.RUnlock()
	if jobperf, ok := qm.actJobs[id]; ok {
		copy := qm.copyJobPerf(jobperf)
		return copy, true
	}
	return nil, false
}

func (qm *ServerMgr) GetActiveJobs() (ajobs map[string]bool) {
	qm.ajLock.RLock()
	defer qm.ajLock.RUnlock()
	ajobs = make(map[string]bool)
	for id, _ := range qm.actJobs {
		ajobs[id] = true
	}
	return
}

func (qm *ServerMgr) removeActJob(id string) {
	qm.ajLock.Lock()
	delete(qm.actJobs, id)
	qm.ajLock.Unlock()
}

func (qm *ServerMgr) isActJob(id string) (ok bool) {
	qm.ajLock.RLock()
	defer qm.ajLock.RUnlock()
	_, ok = qm.actJobs[id]
	return
}

//--------server methods-------

//poll ready tasks and push into workQueue
func (qm *ServerMgr) updateQueue() (err error) {

	logger.Debug(3, "(updateQueue) wait for lock")
	qm.queueLock.Lock()
	defer qm.queueLock.Unlock()

	logger.Debug(3, "(updateQueue) starting")
	var tasks []*Task
	tasks, err = qm.TaskMap.GetTasks()
	if err != nil {
		return
	}
	logger.Debug(3, "(updateQueue) range tasks (%d)", len(tasks))
	for _, task := range tasks {

		var task_id Task_Unique_Identifier
		task_id, err = task.GetId("updateQueue")
		if err != nil {
			return
		}

		var task_state string
		task_state, err = task.GetState()
		if err != nil {
			err = nil
			continue
		}

		if !(task_state == TASK_STAT_INIT || task_state == TASK_STAT_PENDING || task_state == TASK_STAT_READY) {
			logger.Debug(3, "(updateQueue) skipping task %s , it has state %s", task_id, task_state)
			continue
		}

		logger.Debug(3, "(updateQueue) task: %s", task_id)
		var task_ready bool
		var reason string
		task_ready, reason, err = qm.isTaskReady(task)
		if err != nil {
			logger.Error("(updateQueue) %s isTaskReady returns error: %s", task_id, err.Error())
			err = nil
			continue
		}

		if task_ready {
			logger.Debug(3, "(updateQueue) task ready: %s", task_id)

			var job_id string
			job_id, err = task.GetJobId()
			if err != nil {
				err = nil
				continue
			}

			var job *Job
			job, err = GetJob(job_id)
			if err != nil {
				err = nil
				continue
			}

			xerr := qm.taskEnQueue(task, job)
			if xerr != nil {
				logger.Error("(updateQueue) taskEnQueue returned: %s", xerr.Error())
				_ = task.SetState(TASK_STAT_SUSPEND, true)

				job_id, err = task.GetJobId()
				if err != nil {
					return
				}

				var task_str string
				task_str, err = task.String()
				if err != nil {
					err = fmt.Errorf("(updateQueue) task.String returned: %s", err.Error())
					return

				}

				jerror := &JobError{
					TaskFailed:  task_str,
					ServerNotes: "failed enqueuing task, err=" + xerr.Error(),
					Status:      JOB_STAT_SUSPEND,
				}
				if err = qm.SuspendJob(job_id, jerror); err != nil {
					logger.Error("(updateQueue:SuspendJob) job_id=%s; err=%s", job_id, err.Error())
				}
				continue
			}
			logger.Debug(3, "(updateQueue) task enqueued: %s", task_id)
		} else {
			logger.Debug(3, "(updateQueue) task not ready: %s reason: %s", task_id, reason)
		}
	}

	logger.Debug(3, "(updateQueue) range qm.workQueue.Clean()")
	for _, workunit := range qm.workQueue.Clean() {
		id := workunit.Id
		job_id := workunit.JobId
		task_id := workunit.TaskName

		jerror := &JobError{
			WorkFailed:  id,
			TaskFailed:  task_id,
			ServerNotes: "workunit is nil",
			Status:      JOB_STAT_SUSPEND,
		}
		if err = qm.SuspendJob(job_id, jerror); err != nil {
			logger.Error("(updateQueue:SuspendJob) job_id=%s; err=%s", job_id, err.Error())
		}
		logger.Error("(updateQueue) workunit %s is nil, suspending job %s", id, job_id)
	}

	logger.Debug(3, "(updateQueue) ending")

	return
}

func RemoveWorkFromClient(client *Client, workid Workunit_Unique_Identifier) (err error) {
	err = client.Assigned_work.Delete(workid, true)
	if err != nil {
		return
	}

	work_length, err := client.Assigned_work.Length(true)
	if err != nil {
		return
	}

	if work_length > 0 {

		clientid, _ := client.Get_Id(true)

		logger.Error("(RemoveWorkFromClient) Client %s still has %d workunits assigned, after delivering one workunit", clientid, work_length)

		assigned_work_ids, err := client.Assigned_work.Get_list(true)
		if err != nil {
			return err
		}
		for _, work_id := range assigned_work_ids {
			_ = client.Assigned_work.Delete(work_id, true)
		}

		work_length, err = client.Assigned_work.Length(true)
		if err != nil {
			return err
		}
		if work_length > 0 {
			logger.Error("(RemoveWorkFromClient) Client still has work assigned, even after everything should have been deleted.")
			return fmt.Errorf("(RemoveWorkFromClient) Client %s still has %d workunits", clientid, work_length)
		}
	}
	return
}

func (qm *ServerMgr) handleWorkStatDone(client *Client, clientid string, task *Task, workid Workunit_Unique_Identifier, computetime int) (err error) {
	//log event about work done (WD)

	var work_str string
	work_str, err = workid.String()
	if err != nil {
		err = fmt.Errorf("(handleWorkStatDone) workid.String() returned: %s", err.Error())
		return
	}
	//workid_string := workid.String()

	logger.Event(event.WORK_DONE, "workid="+work_str+";clientid="+clientid)
	//update client status

	var task_str string
	task_str, err = task.String()
	if err != nil {
		return
	}

	defer func() {
		//done, remove from the workQueue
		qm.workQueue.Delete(workid)
	}()

	err = client.Increment_total_completed()
	if err != nil {
		err = fmt.Errorf("(handleWorkStatDone) client.Increment_total_completed returned: %s", err.Error())
		return
	}
	var remain_work int
	remain_work, err = task.IncrementRemainWork(-1, true)
	if err != nil {
		err = fmt.Errorf("(handleWorkStatDone) client=%s work=%s %s", clientid, work_str, err.Error())
		return
	}

	// double check, remain_work should equal # of workunits in workqueue with same job and task ids
	workunits, werr := qm.workQueue.GetAll()
	if werr != nil {
		err = fmt.Errorf("(handleWorkStatDone) unable to get workunit list: %s", werr.Error())
		return
	}

	var workunit_count int
	for _, wu := range workunits {
		if (wu.JobId == workid.JobId) && (wu.TaskName == workid.TaskName) && (wu.Rank != workid.Rank) {
			workunit_count += 1
		}
	}
	if workunit_count != remain_work {
		err = fmt.Errorf("(handleWorkStatDone) client=%s work=%s remainwork (%d) does not match number of workunits in queue (%d)", clientid, work_str, remain_work, workunit_count)
		return
	}

	err = task.IncrementComputeTime(computetime)
	if err != nil {
		err = fmt.Errorf("(handleWorkStatDone) client=%s work=%s %s", clientid, work_str, err.Error())
		return
	}

	logger.Debug(3, "(handleWorkStatDone) remain_work: %d (%s)", remain_work, work_str)

	if remain_work > 0 {
		return
	}

	// ******* LAST WORKUNIT ******

	// validate file sizes of all outputs
	verr := task.ValidateOutputs()
	if verr != nil {
		// we create job error object and suspend job
		err_msg := fmt.Sprintf("(handleWorkStatDone) ValidateOutputs failed: %s", verr.Error())
		jerror := &JobError{
			ClientFailed: clientid,
			WorkFailed:   work_str,
			TaskFailed:   task_str,
			ServerNotes:  err_msg,
			Status:       JOB_STAT_SUSPEND,
		}
		err = task.SetState(TASK_STAT_SUSPEND, true)
		if err != nil {
			err = fmt.Errorf("(handleWorkStatDone) task.SetState failed: %s", err.Error())
			return
		}
		err = qm.SuspendJob(task.JobId, jerror)
		if err != nil {
			err = fmt.Errorf("(handleWorkStatDone) SuspendJob failed: %s", err.Error())
			return
		}
		err = errors.New(err_msg)
		return
	}

	err = task.SetState(TASK_STAT_COMPLETED, true)
	if err != nil {
		err = fmt.Errorf("(handleWorkStatDone) task.SetState failed: %s", err.Error())
		return
	}

	//log event about task done (TD)
	qm.FinalizeTaskPerf(task)
	logger.Event(event.TASK_DONE, "task_id="+task_str)

	//update the info of the job which the task is belong to, could result in deletion of the
	//task in the task map when the task is the final task of the job to be done.
	err = qm.updateJobTask(task) //task state QUEUED -> COMPLETED
	if err != nil {
		err = fmt.Errorf("(handleWorkStatDone) updateJobTask failed: %s", err.Error())
	}
	return
}

//handle feedback from a client about the execution of a workunit
func (qm *ServerMgr) handleNoticeWorkDelivered(notice Notice) (err error) {

	work_id := notice.Id
	task_id := work_id.GetTask()

	job_id := work_id.JobId

	status := notice.Status
	clientid := notice.WorkerId
	computetime := notice.ComputeTime
	notes := notice.Notes

	var work_str string
	work_str, err = work_id.String()
	if err != nil {
		err = fmt.Errorf("(handleNoticeWorkDelivered) work_id.String() returned: %s", err.Error())
		return
	}

	logger.Debug(3, "(handleNoticeWorkDelivered) workid: %s status: %s client: %s", work_str, status, clientid)

	// we should not get here, but if we do than end
	if status == WORK_STAT_DISCARDED {
		logger.Error("(handleNoticeWorkDelivered) [warning] skip status change: workid=%s status=%s", work_str, status)
		return
	}

	// *** Get Client
	var client *Client
	var ok bool
	client, ok, err = qm.GetClient(clientid, true)
	if err != nil {
		return
	}
	if !ok {
		err = fmt.Errorf("(handleNoticeWorkDelivered) client not found")
		return
	}
	defer RemoveWorkFromClient(client, work_id)

	// *** Get Task
	var task *Task
	var tok bool
	task, tok, err = qm.TaskMap.Get(task_id, true)
	if err != nil {
		return
	}
	if !tok {
		//task not existed, possible when job is deleted before the workunit done
		err = fmt.Errorf("(handleNoticeWorkDelivered) task %s for workunit %s not found", task_id, work_str)
		logger.Error(err.Error())
		qm.workQueue.Delete(work_id)
		return
	}

	if notice.Results != nil { // TODO one workunit vs multiple !!!!!!!!!!!!!!!!!!!!!!!!!!!!!
		err = task.SetStepOutput(notice.Results, true)
		if err != nil {
			return
		}
	}

	// *** Get workunit
	var work *Workunit
	var wok bool
	work, wok, err = qm.workQueue.Get(work_id)
	if err != nil {
		return
	}
	if !wok {
		err = fmt.Errorf("(handleNoticeWorkDelivered) workunit %s not found in workQueue", work_str)
		return
	}
	if work.State != WORK_STAT_CHECKOUT && work.State != WORK_STAT_RESERVED {
		err = fmt.Errorf("(handleNoticeWorkDelivered) workunit %s did not have state WORK_STAT_CHECKOUT or WORK_STAT_RESERVED (state is %s)", work_str, work.State)
		return
	}

	reason := ""

	if status == WORK_STAT_SUSPEND {
		reason = "workunit suspended by worker" // TODO add more info from worker
	}

	// *** update state of workunit
	err = qm.workQueue.StatusChange(Workunit_Unique_Identifier{}, work, status, reason)
	if err != nil {
		return
	}

	err = task.LockNamed("handleNoticeWorkDelivered/noretry")
	if err != nil {
		return
	}
	noretry := task.Info.NoRetry
	task.Unlock()

	var MAX_FAILURE int
	if noretry == true {
		MAX_FAILURE = 1
	} else {
		MAX_FAILURE = conf.MAX_WORK_FAILURE
	}

	var task_state string
	task_state, err = task.GetState()
	if err != nil {
		return
	}

	if task_state == TASK_STAT_FAIL_SKIP {
		// A work unit for this task failed before this one arrived.
		// User set Skip=2 so the task was just skipped. Any subsiquent
		// workunits are just deleted...
		qm.workQueue.Delete(work_id)
		err = fmt.Errorf("(handleNoticeWorkDelivered) workunit %s failed due to skip", work_str)
		return
	}

	logger.Debug(3, "(handleNoticeWorkDelivered) handling status %s", status)
	if status == WORK_STAT_DONE {
		err = qm.handleWorkStatDone(client, clientid, task, work_id, computetime)
		if err != nil {
			err = fmt.Errorf("(handleNoticeWorkDelivered) handleWorkStatDone returned: %s", err.Error())
			return
		}
	} else if status == WORK_STAT_FAILED_PERMANENT { // (special case !) failed and cannot be recovered

		logger.Event(event.WORK_FAILED, "workid="+work_str+";clientid="+clientid)
		logger.Debug(3, "(handleNoticeWorkDelivered) work failed (status=%s) workid=%s clientid=%s", status, work_str, clientid)
		work.Failed += 1

		qm.workQueue.StatusChange(Workunit_Unique_Identifier{}, work, WORK_STAT_FAILED_PERMANENT, "")

		if err = task.SetState(TASK_STAT_FAILED_PERMANENT, true); err != nil {
			return
		}

		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("(handleNoticeWorkDelivered) task.String returned: %s", err.Error())
			return
		}

		jerror := &JobError{
			ClientFailed: clientid,
			WorkFailed:   work_str,
			TaskFailed:   task_str,
			ServerNotes:  "exit code 42 encountered",
			WorkNotes:    notes,
			AppError:     notice.Stderr,
			Status:       JOB_STAT_FAILED_PERMANENT,
		}
		if err = qm.SuspendJob(job_id, jerror); err != nil {
			logger.Error("(handleNoticeWorkDelivered:SuspendJob) job_id=%s; err=%s", job_id, err.Error())
		}
	} else if status == WORK_STAT_ERROR { //workunit failed, requeue or put it to suspend list
		logger.Event(event.WORK_FAIL, "workid="+work_str+";clientid="+clientid)
		logger.Debug(3, "(handleNoticeWorkDelivered) work failed (status=%s, notes: %s) workid=%s clientid=%s", status, notes, work_str, clientid)

		work.Failed += 1

		if work.Failed < MAX_FAILURE {
			qm.workQueue.StatusChange(Workunit_Unique_Identifier{}, work, WORK_STAT_QUEUED, "")
			logger.Event(event.WORK_REQUEUE, "workid="+work_str)
		} else {
			//failure time exceeds limit, suspend workunit, task, job
			qm.workQueue.StatusChange(Workunit_Unique_Identifier{}, work, WORK_STAT_SUSPEND, "work.Failed >= MAX_FAILURE")
			logger.Event(event.WORK_SUSPEND, "workid="+work_str)

			if err = task.SetState(TASK_STAT_SUSPEND, true); err != nil {
				return
			}

			var task_str string
			task_str, err = task.String()
			if err != nil {
				err = fmt.Errorf("(handleNoticeWorkDelivered) task.String returned: %s", err.Error())
				return
			}
			jerror := &JobError{
				ClientFailed: clientid,
				WorkFailed:   work_str,
				TaskFailed:   task_str,
				ServerNotes:  fmt.Sprintf("workunit failed %d time(s)", MAX_FAILURE),
				WorkNotes:    notes,
				AppError:     notice.Stderr,
				Status:       JOB_STAT_SUSPEND,
			}
			if err = qm.SuspendJob(job_id, jerror); err != nil {
				logger.Error("(handleNoticeWorkDelivered:SuspendJob) job_id=%s; err=%s", job_id, err.Error())
			}
		}

		// Suspend client if needed
		var client *Client
		var ok bool
		client, ok, err = qm.GetClient(clientid, true)
		if err != nil {
			return
		}
		if !ok {
			err = fmt.Errorf(e.ClientNotFound)
			return
		}

		err = client.Append_Skip_work(work_id, true)
		if err != nil {
			return
		}
		err = client.Increment_total_failed(true)
		if err != nil {
			return
		}

		var last_failed int
		last_failed, err = client.Increment_last_failed(true)
		if err != nil {
			return
		}
		if last_failed >= conf.MAX_CLIENT_FAILURE {
			qm.SuspendClient(clientid, client, "MAX_CLIENT_FAILURE on client reached", true)
		}
	} else {
		err = fmt.Errorf("No handler for workunit status '%s' implemented (allowd: %s, %s, %s)", status, WORK_STAT_DONE, WORK_STAT_FAILED_PERMANENT, WORK_STAT_ERROR)
		return
	}
	return
}

func (qm *ServerMgr) GetJsonStatus() (status map[string]map[string]int, err error) {
	start := time.Now()
	queuing_work, err := qm.workQueue.Queue.Len()
	if err != nil {
		return
	}
	out_work, err := qm.workQueue.Checkout.Len()
	if err != nil {
		return
	}
	suspend_work, err := qm.workQueue.Suspend.Len()
	if err != nil {
		return
	}
	total_active_work, err := qm.workQueue.Len()
	if err != nil {
		return
	}
	elapsed := time.Since(start)
	logger.Debug(3, "time GetJsonStatus/Len: %s", elapsed)

	total_task := 0
	queuing_task := 0
	started_task := 0
	pending_task := 0
	completed_task := 0
	suspended_task := 0
	skipped_task := 0
	fail_skip_task := 0

	start = time.Now()
	task_list, err := qm.TaskMap.GetTasks()
	if err != nil {
		return
	}
	elapsed = time.Since(start)
	logger.Debug(3, "time GetJsonStatus/GetTasks: %s", elapsed)

	start = time.Now()
	for _, task := range task_list {
		total_task += 1

		var task_state string
		task_state, err = task.GetState()
		if err != nil {
			return
		}

		switch task_state {
		case TASK_STAT_COMPLETED:
			completed_task += 1
		case TASK_STAT_PENDING:
			pending_task += 1
		case TASK_STAT_QUEUED:
			queuing_task += 1
		case TASK_STAT_INPROGRESS:
			started_task += 1
		case TASK_STAT_SUSPEND:
			suspended_task += 1
		case TASK_STAT_SKIPPED:
			skipped_task += 1
		case TASK_STAT_FAIL_SKIP:
			fail_skip_task += 1
		}
	}
	elapsed = time.Since(start)
	logger.Debug(3, "time GetJsonStatus/task_list: %s", elapsed)

	total_task -= skipped_task // user doesn't see skipped tasks
	active_jobs := qm.lenActJobs()
	suspend_job := qm.lenSusJobs()
	total_job := active_jobs + suspend_job
	total_client := 0
	busy_client := 0
	idle_client := 0
	suspend_client := 0

	start = time.Now()
	client_list, err := qm.clientMap.GetClients()
	if err != nil {
		return
	}
	total_client = len(client_list)
	elapsed = time.Since(start)
	logger.Debug(3, "time GetJsonStatus/GetClients: %s", elapsed)

	start = time.Now()

	for _, client := range client_list {
		rlock, err := client.RLockNamed("GetJsonStatus")
		if err != nil {
			continue
		}

		if client.Suspended {
			suspend_client += 1
		}
		if client.Busy {
			busy_client += 1
		} else {
			idle_client += 1
		}

		client.RUnlockNamed(rlock)

	}
	elapsed = time.Since(start)
	logger.Debug(3, "time GetJsonStatus/client_list: %s", elapsed)

	jobs := map[string]int{
		"total":     total_job,
		"active":    active_jobs,
		"suspended": suspend_job,
	}
	tasks := map[string]int{
		"total":       total_task,
		"queuing":     queuing_task,
		"in-progress": started_task,
		"pending":     pending_task,
		"completed":   completed_task,
		"suspended":   suspended_task,
		"failed":      fail_skip_task,
	}
	workunits := map[string]int{
		"total":     total_active_work,
		"queuing":   queuing_work,
		"checkout":  out_work,
		"suspended": suspend_work,
	}
	clients := map[string]int{
		"total":     total_client,
		"busy":      busy_client,
		"idle":      idle_client,
		"suspended": suspend_client,
	}
	status = map[string]map[string]int{
		"jobs":      jobs,
		"tasks":     tasks,
		"workunits": workunits,
		"clients":   clients,
	}
	return
}

func (qm *ServerMgr) GetTextStatus() string {
	status, _ := qm.GetJsonStatus() // TODO handle error
	statMsg := "++++++++AWE server queue status++++++++\n" +
		fmt.Sprintf("total jobs ............... %d\n", status["jobs"]["total"]) +
		fmt.Sprintf("    active:           (%d)\n", status["jobs"]["active"]) +
		fmt.Sprintf("    suspended:        (%d)\n", status["jobs"]["suspended"]) +
		fmt.Sprintf("total tasks .............. %d\n", status["tasks"]["total"]) +
		fmt.Sprintf("    queuing:          (%d)\n", status["tasks"]["queuing"]) +
		fmt.Sprintf("    in-progress:      (%d)\n", status["tasks"]["in-progress"]) +
		fmt.Sprintf("    pending:          (%d)\n", status["tasks"]["pending"]) +
		fmt.Sprintf("    completed:        (%d)\n", status["tasks"]["completed"]) +
		fmt.Sprintf("    suspended:        (%d)\n", status["tasks"]["suspended"]) +
		fmt.Sprintf("    failed & skipped: (%d)\n", status["tasks"]["failed"]) +
		fmt.Sprintf("total workunits .......... %d\n", status["workunits"]["total"]) +
		fmt.Sprintf("    queuing:          (%d)\n", status["workunits"]["queuing"]) +
		fmt.Sprintf("    checkout:         (%d)\n", status["workunits"]["checkout"]) +
		fmt.Sprintf("    suspended:        (%d)\n", status["workunits"]["suspended"]) +
		fmt.Sprintf("total clients ............ %d\n", status["clients"]["total"]) +
		fmt.Sprintf("    busy:             (%d)\n", status["clients"]["busy"]) +
		fmt.Sprintf("    idle:             (%d)\n", status["clients"]["idle"]) +
		fmt.Sprintf("    suspend:          (%d)\n", status["clients"]["suspended"]) +
		fmt.Sprintf("---last update: %s\n\n", time.Now())
	return statMsg
}

//---end of mgr methods

//--workunit methds (servermgr implementation)
func (qm *ServerMgr) FetchDataToken(work_id Workunit_Unique_Identifier, clientid string) (token string, err error) {

	//precheck if the client is registered
	client, ok, err := qm.GetClient(clientid, true)
	if err != nil {
		return
	}
	if !ok {
		return "", errors.New(e.ClientNotFound)
	}

	is_suspended, err := client.Get_Suspended(true)
	if err != nil {
		return
	}

	if is_suspended {
		err = errors.New(e.ClientSuspended)
		return
	}

	jobid := work_id.JobId

	job, err := GetJob(jobid)
	if err != nil {
		return
	}
	token = job.GetDataToken()
	if token == "" {
		var work_str string
		work_str, err = work_id.String()
		if err != nil {
			err = fmt.Errorf("(FetchDataToken) workid.String() returned: %s", err.Error())
			return
		}
		err = errors.New("no data token set for workunit " + work_str)
		return
	}
	return
}

// func (qm *ServerMgr) FetchPrivateEnvs_deprecated(workid string, clientid string) (envs map[string]string, err error) {
// 	//precheck if the client is registered
// 	client, ok, err := qm.GetClient(clientid, true)
// 	if err != nil {
// 		return
// 	}
// 	if !ok {
// 		return nil, errors.New(e.ClientNotFound)
// 	}
// 	client_status, err := client.Get_Status(true)
// 	if err != nil {
// 		return
// 	}
// 	if client_status == CLIENT_STAT_SUSPEND {
// 		return nil, errors.New(e.ClientSuspended)
// 	}
// 	jobid, err := GetJobIdByWorkId(workid)
// 	if err != nil {
// 		return nil, err
// 	}
//
// 	job, err := GetJob(jobid)
// 	if err != nil {
// 		return nil, err
// 	}
//
// 	taskid, _ := GetTaskIdByWorkId(workid)
//
// 	idx := -1
// 	for i, t := range job.Tasks {
// 		if t.Id == taskid {
// 			idx = i
// 			break
// 		}
// 	}
// 	envs = job.Tasks[idx].Cmd.Environ.Private
// 	if envs == nil {
// 		return nil, errors.New("no private envs for workunit " + workid)
// 	}
// 	return envs, nil
// }

func (qm *ServerMgr) SaveStdLog(id Workunit_Unique_Identifier, logname string, tmppath string) (err error) {
	savedpath, err := getStdLogPathByWorkId(id, logname)
	if err != nil {
		return err
	}
	os.Rename(tmppath, savedpath)
	return
}

func (qm *ServerMgr) GetReportMsg(id Workunit_Unique_Identifier, logname string) (report string, err error) {
	logpath, err := getStdLogPathByWorkId(id, logname)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(logpath); err != nil {
		return "", errors.New("log type '" + logname + "' not found")
	}

	content, err := ioutil.ReadFile(logpath)
	if err != nil {
		return "", err
	}
	return string(content), err
}

func getStdLogPathByWorkId(id Workunit_Unique_Identifier, logname string) (savedpath string, err error) {
	jobid := id.JobId

	var logdir string
	logdir, err = getPathByJobId(jobid)
	if err != nil {
		return
	}
	//workid := id.String()
	var work_str string
	work_str, err = id.String()
	if err != nil {
		err = fmt.Errorf("(getStdLogPathByWorkId) id.String() returned: %s", err.Error())
		return
	}

	savedpath = fmt.Sprintf("%s/%s.%s", logdir, work_str, logname)
	return
}

//---task methods----
// this is invoked after a job is uploaded and saved in mongo
func (qm *ServerMgr) EnqueueTasksByJobId(jobid string) (err error) {
	logger.Debug(3, "(EnqueueTasksByJobId) starting")
	job, err := GetJob(jobid)
	if err != nil {
		err = fmt.Errorf("(EnqueueTasksByJobId) GetJob failed: %s", err.Error())
		return
	}

	var tasks []*Task
	tasks, err = job.GetTasks()
	if err != nil {
		err = fmt.Errorf("(EnqueueTasksByJobId) job.GetTasks failed: %s", err.Error())
		return
	}

	task_len := len(tasks)

	logger.Debug(3, "(EnqueueTasksByJobId) got %d tasks", task_len)

	err = job.SetState(JOB_STAT_QUEUING, nil)
	if err != nil {
		err = fmt.Errorf("(qmgr.taskEnQueue) UpdateJobState: %s", err.Error())
		return
	}

	qm.CreateJobPerf(jobid)

	for _, task := range tasks {
		var task_state string
		task_state, err = task.GetState()
		if err != nil {
			return
		}

		if task_state == TASK_STAT_INPROGRESS || task_state == TASK_STAT_QUEUED {
			task.SetState(TASK_STAT_READY, true)
		} else if task_state == TASK_STAT_SUSPEND {
			task.SetState(TASK_STAT_PENDING, true)
		}

		err = qm.addTask(task, job)
		if err != nil {
			err = fmt.Errorf("(EnqueueTasksByJobId) addTask failed: %s", err.Error())
			return
		}
	}

	var job_state string
	job_state, err = job.GetState(true)
	if err != nil {
		return
	}
	if job_state != JOB_STAT_INPROGRESS {
		err = job.SetState(JOB_STAT_QUEUED, []string{JOB_STAT_INIT, JOB_STAT_SUSPEND, JOB_STAT_QUEUING})
		if err != nil {
			return
		}
	}

	return
}

//---end of task methods

func (qm *ServerMgr) addTask(task *Task, job *Job) (err error) {
	logger.Debug(3, "(addTask) got task")

	var task_id Task_Unique_Identifier
	task_id, err = task.GetId("addTask")
	if err != nil {
		err = fmt.Errorf("(addTask) GetId() returns: %s", err.Error())
		return
	}

	var task_state string
	task_state, err = task.GetState()
	if err != nil {
		err = fmt.Errorf("(addTask) task.GetState() returns: %s", err.Error())
		return
	}
	logger.Debug(3, "(addTask) state of task: %s", task_state)

	// TaskMap.Add - makes it a pending task if init, throws error if task already in map with different pointer
	var modified bool
	modified, err = qm.TaskMap.Add(task)
	if err != nil {
		err = fmt.Errorf("(addTask) qm.TaskMap.Add() returns: %s", err.Error())
		return
	}

	// don't enqueue completed or passed
	if (task_state == TASK_STAT_COMPLETED) || (task_state == TASK_STAT_PASSED) {
		return
	}

	if modified {
		task_state, err = task.GetState()
		if err != nil {
			err = fmt.Errorf("(addTask) task.GetState() returns: %s", err.Error())
			return
		}
	}

	var task_ready bool
	task_ready, _, err = qm.isTaskReady(task) // makes the task ready
	if err != nil {
		err = fmt.Errorf("(addTask) qm.isTaskReady(task) returns: %s", err.Error())
		return
	}
	if !task_ready {
		return
	}

	logger.Debug(3, "(addTask) task %s is ready (invoking taskEnQueue)", task_id)
	xerr := qm.taskEnQueue(task, job)
	if xerr != nil {
		logger.Error("(addTask) taskEnQueue returned error: %s", xerr.Error())
		_ = task.SetState(TASK_STAT_SUSPEND, true)

		var job_id string
		job_id, err = task.GetJobId()
		if err != nil {
			return
		}

		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("(addTask) task.String returned: %s", err.Error())
			return
		}
		jerror := &JobError{
			TaskFailed:  task_str,
			ServerNotes: "failed in enqueuing task, err=" + xerr.Error(),
			Status:      JOB_STAT_SUSPEND,
		}

		if err = qm.SuspendJob(job_id, jerror); err != nil {
			logger.Error("(updateQueue:SuspendJob) job_id=%s; err=%s", job_id, err.Error())
		}
		err = xerr
		return
	}

	err = qm.updateJobTask(task) //task state INIT->PENDING
	if err != nil {
		err = fmt.Errorf("(addTask) updateJobTask returned: %s", err.Error())
		//fmt.Println(err.Error())
		return
	}
	logger.Debug(3, "(addTask) leaving...")
	return
}

//check whether a pending task is ready to enqueue (dependent tasks are all done)
// task is not locked
func (qm *ServerMgr) isTaskReady(task *Task) (ready bool, reason string, err error) {
	ready = false

	logger.Debug(3, "(isTaskReady) starting")

	task_state, err := task.GetStateNamed("isTaskReady")
	if err != nil {
		return
	}

	logger.Debug(3, "(isTaskReady) task state is %s", task_state)

	if task_state == TASK_STAT_READY {
		ready = true
		return
	}

	if task_state == TASK_STAT_INIT || task_state == TASK_STAT_PENDING {
		logger.Debug(3, "(isTaskReady) task state is %s", task_state)
	} else {
		err = fmt.Errorf("(isTaskReady) task has state %s, it does not make sense to test if it is ready", task_state)
		return
	}

	task_id, err := task.GetId("isTaskReady")
	if err != nil {
		return
	}
	logger.Debug(3, "(isTaskReady %s)", task_id)

	//skip if the belonging job is suspended
	jobid, err := task.GetJobId()
	if err != nil {
		return
	}

	job, err := GetJob(jobid)
	if err != nil {
		return
	}
	job_state, err := job.GetState(true)
	if err != nil {
		return
	}
	if job_state == JOB_STAT_SUSPEND {
		reason = "job is suspend"
		return
	}

	if task.Info != nil {
		info := task.Info
		if !info.StartAt.IsZero() {

			if info.StartAt.After(time.Now()) {
				// too early
				logger.Debug(3, "(isTaskReady %s) too early to execute (now: %s, StartAt: %s)", task_id, time.Now(), info.StartAt)
				return
			} else {
				logger.Debug(3, "(isTaskReady %s) StartAt field is in the past, can execute now (now: %s, StartAt: %s)", task_id, time.Now(), info.StartAt)
			}
		}
	}

	if task.WorkflowStep != nil {
		// check if CWL-style predecessors are all TASK_STAT_COMPLETED
		// ****** get inputs
		if job == nil {
			err = fmt.Errorf("(isTaskReady) job == nil")
			return
		}

		if job.CWL_collection == nil {
			err = fmt.Errorf("(isTaskReady) job.CWL_collection == nil")
			return
		}

		job_input_map := *job.CWL_collection.Job_input_map
		if job_input_map == nil {
			err = fmt.Errorf("(isTaskReady) job.CWL_collection.Job_input_map is empty")
			return
		}

		var workflow_instance *WorkflowInstance
		workflow_instance, err = job.GetWorkflowInstance(task.Parent, true)
		if err != nil {
			err = fmt.Errorf("(isTaskReady) GetWorkflowInstance returned %s", err.Error())
			return
		}
		workflow_input_map := workflow_instance.Inputs.GetMap()

		fmt.Println("WorkflowStep.Id: " + task.WorkflowStep.Id)
		for _, wsi := range task.WorkflowStep.In { // WorkflowStepInput
			if wsi.Source == nil {
				if wsi.Default != nil { // input is optional, anyway....
					continue
				}
				reason = fmt.Sprintf("(isTaskReady) No Source and no default found")
			} else {
				source_is_array := false
				source_as_array, source_is_array := wsi.Source.([]interface{})

				if source_is_array {
					for _, src := range source_as_array { // usually only one
						var src_str string
						var ok bool

						src_str, ok = src.(string)
						if !ok {
							err = fmt.Errorf("src is not a string")
							return
						}

						_, ok, err = qm.getCWLSource(workflow_input_map, job, task_id, src_str, false)
						if err != nil {
							err = fmt.Errorf("(isTaskReady) (type array, src_str: %s) getCWLSource returns: %s", src_str, err.Error())
							return
						}
						if !ok {
							reason = fmt.Sprintf("Source CWL object (type array) %s not found", src_str)
							return
						}
					}
				} else {
					var src_str string
					var ok bool

					src_str, ok = wsi.Source.(string)
					if !ok {
						err = fmt.Errorf("(isTaskReady) Cannot parse WorkflowStep source: %s", spew.Sdump(wsi.Source))
						return
					}

					_, ok, err = qm.getCWLSource(workflow_input_map, job, task_id, src_str, false)
					if err != nil {
						err = fmt.Errorf("(isTaskReady) (type non-array, src_str: %s) getCWLSource returns: %s", src_str, err.Error())
						return
					}
					if !ok {
						reason = fmt.Sprintf("Source CWL object (type non-array) %s not found", src_str)
						return
					}
				}
			}
		}
	}

	if task.WorkflowStep == nil {
		// task read lock, check DependsOn list and IO.Origin list
		reason, err = task.ValidateDependants(qm)
		if err != nil {
			err = fmt.Errorf("(isTaskReady) %s", err.Error())
			return
		}
		if reason != "" {
			return
		}
	}

	if task_state == TASK_STAT_PENDING {
		err = task.SetState(TASK_STAT_READY, true)
		if err != nil {
			return
		}
	}

	ready = true
	logger.Debug(3, "(isTaskReady) finished, task %s is ready", task_id)
	return
}

// happens when task is ready
// prepares task and creates workunits
// scatter task does not create its own workunit, it just creates new tasks
func (qm *ServerMgr) taskEnQueue(task *Task, job *Job) (err error) {

	var task_id Task_Unique_Identifier
	task_id, err = task.GetId("taskEnQueue")
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) Could not get Id: %s", err.Error())
		return
	}

	var state string
	state, err = task.GetState()
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) Could not get State: %s", err.Error())
		return
	}

	if state != TASK_STAT_READY {
		err = fmt.Errorf("(taskEnQueue) Task state should be TASK_STAT_READY, got state %s", state)
		return
	}

	if task.WorkflowStep != nil {
		logger.Debug(3, "(taskEnQueue) have WorkflowStep")
	} else {
		logger.Debug(3, "(taskEnQueue) DO NOT have WorkflowStep")
	}

	skip_workunit := false

	var task_type string
	task_type, err = task.GetTaskType()
	if err != nil {
		return
	}

	if job.CWL_collection != nil {
		logger.Debug(3, "(taskEnQueue) have job.CWL_collection")

		var workflow_instance *WorkflowInstance
		workflow_instance, err = job.GetWorkflowInstance(task.Parent, true)
		if err != nil {
			err = fmt.Errorf("(taskEnQueue) GetWorkflowInstance returned %s", err.Error())
			return
		}

		workflow_input_map := workflow_instance.Inputs.GetMap()
		cwl_step := task.WorkflowStep

		// scatter
		if task_type == "" {
			if len(cwl_step.Scatter) != 0 {
				err = fmt.Errorf("Scatter not implemented yet")
				return // TODO

				task_type = TASK_TYPE_SCATTER
				err = task.SetTaskType(task_type, true)
				if err != nil {
					return
				}

				err = task.SetState(TASK_STAT_QUEUED, true) // this refers to the fact that its scatter children have been enqueued.
				if err != nil {
					return
				}
			}

		}

		// Sub-workflow
		for task_type == "" || task_type == TASK_TYPE_WORKFLOW {

			// sub workflow tasks have already been created.
			if task.Children != nil && len(task.Children) != 0 {
				// fix type
				if task_type == "" {
					task_type = TASK_TYPE_WORKFLOW
					err = task.SetTaskType(task_type, true)
					if err != nil {
						return
					}
				}
				break
			}

			// not sure if this is a subworkflow
			p := cwl_step.Run
			if p == nil {
				err = fmt.Errorf("(taskEnQueue) process is nil !?")
				return
			}

			var schemata []cwl.CWLType_Type
			schemata, err = job.CWL_collection.GetSchemata()
			if err != nil {
				err = fmt.Errorf("(updateJobTask) job.CWL_collection.GetSchemata() returned: %s", err.Error())
				return
			}

			// check if this is a workflow
			//var process_name string
			//var a_command_line_tool *cwl.CommandLineTool
			//wfl = nil
			var process interface{}
			process, _, err = cwl.GetProcess(p, job.CWL_collection, job.CwlVersion, schemata) // TODO add new_schemata
			if err != nil {
				err = fmt.Errorf("(taskEnQueue) cwl.GetProcess returned: %s (task_type=%s)", err.Error(), task_type)
				return
			}

			var wfl *cwl.Workflow
			var ok bool
			wfl, ok = process.(*cwl.Workflow)

			if !ok {
				// this must CommandLineTool or ExpressionTool
				task_type = TASK_TYPE_NORMAL
				err = task.SetTaskType(task_type, true)
				if err != nil {
					return
				}
				break
			}

			// we got a Sub-workflow !
			task_type = TASK_TYPE_WORKFLOW
			err = task.SetTaskType(task_type, true)
			if err != nil {
				return
			}

			// find inputs
			var task_input_array cwl.Job_document
			var task_input_map cwl.JobDocMap

			if task.StepInput == nil {
				task_input_map, err = qm.GetStepInputObjects(job, task_id, workflow_input_map, cwl_step) // returns map[string]CWLType
				if err != nil {
					return
				}
				task_input_array, err = task_input_map.GetArray()
				if err != nil {
					err = fmt.Errorf("(taskEnQueue) task_input_map.GetArray returned: %s", err.Error())
					return
				}
				task.StepInput = &task_input_array

			} else {
				task_input_array = *task.StepInput
				task_input_map = task_input_array.GetMap()
			}

			if strings.HasSuffix(task.TaskName, "/") {
				err = fmt.Errorf("(taskEnQueue) Slash at the end of TaskName!? %s", task.TaskName)
				return
			}

			if strings.HasSuffix(task.Parent, "/") {
				err = fmt.Errorf("(taskEnQueue) Slash at the end of Parent!? %s", task.Parent)
				return
			}

			new_sub_workflow := ""
			if len(task.Parent) > 0 {
				new_sub_workflow = task.Parent + task.TaskName // TaskName starts with #, so we can split later
			} else {
				new_sub_workflow = task.TaskName
			}

			fmt.Printf("New Subworkflow: %s %s\n", task.Parent, task.TaskName)

			err = job.AddWorkflowInstance(new_sub_workflow, task_input_array, len(wfl.Steps))
			if err != nil {
				return
			}

			// create tasks
			var sub_workflow_tasks []*Task
			sub_workflow_tasks, err = CreateTasks(job, new_sub_workflow, wfl.Steps)

			err = job.IncrementRemainTasks(len(sub_workflow_tasks))
			if err != nil {
				return
			}

			children := []Task_Unique_Identifier{}
			for i := range sub_workflow_tasks {
				sub_task := sub_workflow_tasks[i]
				_, err = sub_task.Init(job)
				if err != nil {
					err = fmt.Errorf("(taskEnQueue) sub_task.Init() returns: %s", err.Error())
					return
				}

				var sub_task_id Task_Unique_Identifier
				sub_task_id, err = sub_task.GetId("taskEnQueue." + strconv.Itoa(i))
				if err != nil {
					return
				}
				children = append(children, sub_task_id)

				err = job.AddTask(sub_task)
				if err != nil {
					err = fmt.Errorf("(taskEnQueue) job.AddTask returns: %s", err.Error())
					return
				}
				err = qm.addTask(sub_task, job)
				if err != nil {
					err = fmt.Errorf("(taskEnQueue) (subtask: %s) qm.addTask returned: %s", sub_task_id, err.Error())
					return
				}

			}
			task.Children = children // TODO lock
			// break (trivial for loop)
		}

	} else {
		logger.Debug(3, "(taskEnQueue) DO NOT have job.CWL_collection")
	}

	logger.Debug(2, "(taskEnQueue) task %s has type %s", task_id, task_type)
	if task_type == TASK_TYPE_WORKFLOW || task_type == TASK_TYPE_SCATTER {
		skip_workunit = true
	}

	logger.Debug(2, "(taskEnQueue) trying to enqueue task %s", task_id)

	// if task was flagged by resume, recompute, or resubmit - reset it
	err = task.SetResetTask(job.Info)
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) SetResetTask: %s", err.Error())
		return
	}

	err = qm.locateInputs(task, job) // only old-style AWE
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) locateInputs: %s", err.Error())
		return
	}

	// init partition
	err = task.InitPartIndex()
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) InitPartitionIndex: %s", err.Error())
		return
	}

	err = qm.createOutputNode(task)
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) createOutputNode: %s", err.Error())
		return
	}

	if !skip_workunit {
		err = qm.CreateAndEnqueueWorkunits(task, job)
		if err != nil {
			err = fmt.Errorf("(taskEnQueue) CreateAndEnqueueWorkunits: %s", err.Error())
			return
		}
	}
	err = task.SetState(TASK_STAT_QUEUED, true)
	if err != nil {
		return
	}
	err = task.SetCreatedDate(time.Now()) // TODO: this is pretty stupid and useless. May want to use EnqueueDate here ?
	if err != nil {
		return
	}
	err = task.SetStartedDate(time.Now()) //TODO: will be changed to the time when the first workunit is checked out
	if err != nil {
		return
	}
	err = qm.updateJobTask(task) //task status PENDING->QUEUED
	if err != nil {
		err = fmt.Errorf("(taskEnQueue) qm.updateJobTask: %s", err.Error())
		return
	}
	// log event about task enqueue (TQ)
	logger.Event(event.TASK_ENQUEUE, fmt.Sprintf("taskid=%s;totalwork=%d", task_id, task.TotalWork))
	qm.CreateTaskPerf(task)

	logger.Debug(2, "(taskEnQueue) leaving (task=%s)", task_id)

	return
}

// invoked by taskEnQueue
// main purpose is to copy output io struct of predecessor task to create the input io structs
func (qm *ServerMgr) locateInputs(task *Task, job *Job) (err error) {
	if task.WorkflowStep != nil && job.CWL_collection != nil {
		if job.CWL_collection.Job_input_map == nil {
			err = fmt.Errorf("job.CWL_collection.Job_input_map is empty")
			return
		}
	} else {
		// old AWE-style
		err = task.ValidateInputs(qm)
		if err != nil {
			return
		}
		err = task.ValidatePredata()
		if err != nil {
			err = fmt.Errorf("ValidatePredata failed: %s", err.Error())
		}
	}
	return
}

// this retrieves the input from either the (sub-)workflow input, or from the output of another task in the same (sub-)workflow
// error_on_missing_task: when checking if a task is ready, a missing task is not an error, it just means task is not ready,
//    but when getting data this is actually an error.
func (qm *ServerMgr) getCWLSource(workflow_input_map map[string]cwl.CWLType, job *Job, current_task_id Task_Unique_Identifier, src string, error_on_missing_task bool) (obj cwl.CWLType, ok bool, err error) {

	ok = false
	//src = strings.TrimPrefix(src, "#main/")

	logger.Debug(3, "(getCWLSource) searching for %s", src)

	src_array := strings.Split(src, "/")
	if len(src_array) == 2 {
		logger.Debug(3, "(getCWLSource) a workflow input")
		// must be a workflow input, e.g. #main/jobid (workflow, input)

		src_base := src_array[1]
		//fmt.Println("src_base: " + src_base)
		// search job input
		var this_ok bool
		obj, this_ok = workflow_input_map[src_base]
		if this_ok {
			fmt.Println("(getCWLSource) found in workflow_input_map: " + src_base)
			ok = true
			return

		} else {

			// workflow inputs that are missing might be optional, thus Null is returned
			obj = cwl.NewNull()
			ok = true
			// not found
			return
		}
		//fmt.Println("(getCWLSource) workflow_input_map:")
		//spew.Dump(workflow_input_map)

	} else if len(src_array) == 3 {
		logger.Debug(3, "(getCWLSource) a step output")
		// must be a step output, e.g. #main/filter/rejected (workflow, step, output)
		workflow_name := src_array[0]
		step_name := src_array[1]
		output_name := src_array[2]
		_ = output_name

		//step_name_abs := workflow_name + "/" + step_name

		ancestor_task_id := current_task_id

		ancestor_task_id.TaskName = workflow_name + "/" + step_name

		var ancestor_task *Task
		var local_ok bool
		ancestor_task, local_ok, err = qm.TaskMap.Get(ancestor_task_id, true)
		if err != nil {
			err = fmt.Errorf("(getCWLSource) qm.TaskMap.Get returned: %s", err.Error())
			return
		}
		if !local_ok {
			if error_on_missing_task {
				err = fmt.Errorf("(getCWLSource) ancestor_task %s not found ", ancestor_task_id)
				return
			}
			logger.Debug(3, "(getCWLSource) ancestor_task %s not found ", ancestor_task_id)
			ok = false
			return
		}

		if ancestor_task == nil {
			err = fmt.Errorf("(getCWLSource) did not find predecessor task %s for task %s", ancestor_task_id, src) // this should not happen, taskReady makes sure everything is available
			return
		}

		if ancestor_task.StepOutput == nil {
			//err = fmt.Errorf("(getCWLSource) Found predecessor task %s, but StepOutput does not exist", step_name_abs)
			logger.Debug(3, "(getCWLSource) ancestor_task.StepOutput == nil")
			ok = false
			return
		}

		logger.Debug(3, "(getCWLSource) len(ancestor_task.StepOutput): %d", len(*ancestor_task.StepOutput))

		for _, named_step_output := range *ancestor_task.StepOutput {

			named_step_output_base := path.Base(named_step_output.Id)

			logger.Debug(3, "(getCWLSource) %s vs %s\n", named_step_output_base, output_name)
			if named_step_output_base == output_name {

				obj = named_step_output.Value

				if obj == nil {
					err = fmt.Errorf("(getCWLSource) found %s , but it is nil", output_name) // this should not happen, taskReady makes sure everything is available
					return
				}

				ok = true

				return

			}

		}

		//err = fmt.Errorf("(getCWLSource) did not find output in predecessor task %s ", step_name) // this should not happen, taskReady makes sure everything is available
		// not found
		logger.Debug(3, "(getCWLSource) step output not found")
		ok = false

	} else {
		err = fmt.Errorf("(getCWLSource) could not parse source: %s", src)

	}

	return
}

func (qm *ServerMgr) GetStepInputObjects(job *Job, task_id Task_Unique_Identifier, workflow_input_map map[string]cwl.CWLType, workflow_step *cwl.WorkflowStep) (workunit_input_map cwl.JobDocMap, err error) {

	workunit_input_map = make(map[string]cwl.CWLType) // also used for json

	//fmt.Println("(GetStepInputObjects) workflow_step.In:")
	//spew.Dump(workflow_step.In)

	// 1. find all object source and Defaut
	// 2. make a map copy to be used in javaqscript, as "inputs"
	for _, input := range workflow_step.In {
		// input is a WorkflowStepInput

		id := input.Id

		cmd_id := path.Base(id)

		// get data from Source, Default or valueFrom

		link_merge_method := ""
		if input.LinkMerge != nil {
			link_merge_method = string(*input.LinkMerge)
			if link_merge_method != "merge_flattened" {
				err = fmt.Errorf("(NewWorkunit) sorry, LinkMergeMethod \"%s\" not supported yet", link_merge_method) // merge_nested merge_flattened
				return
			}

		}

		if input.Source != nil {
			//source_object_array := []cwl.CWLType{}
			//resolve pointers in source

			source_is_array := false

			source_as_string := ""
			source_as_array, source_is_array := input.Source.([]interface{})

			if source_is_array {
				//fmt.Printf("source is a array: %s", spew.Sdump(input.Source))
				cwl_array := cwl.Array{}
				for _, src := range source_as_array { // usually only one
					fmt.Println("src: " + spew.Sdump(src))
					var src_str string
					var ok bool
					src_str, ok = src.(string)
					if !ok {
						err = fmt.Errorf("src is not a string")
						return
					}
					var job_obj cwl.CWLType
					job_obj, ok, err = qm.getCWLSource(workflow_input_map, job, task_id, src_str, true)
					if err != nil {
						err = fmt.Errorf("(GetStepInputObjects) (array) getCWLSource returns: %s", err.Error())
						return
					}
					if !ok {
						err = fmt.Errorf("(GetStepInputObjects) (array) getCWLSource did not find output \"%s\"", src_str)
						return // TODO allow optional ??
					}

					if link_merge_method == "merge_flattened" {

						job_obj_type := job_obj.GetType()

						if job_obj_type != cwl.CWL_array {
							err = fmt.Errorf("(GetStepInputObjects) merge_flattened, expected array as input, but got %s", job_obj_type)
							return
						}

						var an_array *cwl.Array
						an_array, ok = job_obj.(*cwl.Array)
						if !ok {
							err = fmt.Errorf("got type: %s", reflect.TypeOf(job_obj))
							return
						}

						for i, _ := range *an_array {
							//source_object_array = append(source_object_array, (*an_array)[i])
							cwl_array = append(cwl_array, (*an_array)[i])
						}

					} else {
						//source_object_array = append(source_object_array, job_obj)
						cwl_array = append(cwl_array, job_obj)
					}
					//cwl_array = append(cwl_array, obj)
				}

				workunit_input_map[cmd_id] = &cwl_array

			} else {
				//fmt.Printf("source is NOT a array: %s", spew.Sdump(input.Source))
				var ok bool
				source_as_string, ok = input.Source.(string)
				if !ok {
					err = fmt.Errorf("(GetStepInputObjects) (string) Cannot parse WorkflowStep source: %s", spew.Sdump(input.Source))
					return
				}

				var job_obj cwl.CWLType
				job_obj, ok, err = qm.getCWLSource(workflow_input_map, job, task_id, source_as_string, true)
				if err != nil {
					err = fmt.Errorf("(GetStepInputObjects) (string) getCWLSource returns: %s", err.Error())
					return
				}
				if !ok {
					if input.Default == nil {
						err = fmt.Errorf("(GetStepInputObjects) (string) getCWLSource did not find output (nor a default) that can be used as input \"%s\"", source_as_string)
						return
					}
					job_obj, err = cwl.NewCWLType("", input.Default)
					if err != nil {
						err = fmt.Errorf("(GetStepInputObjects) could not use default: %s", err.Error())
						return
					}

				}
				workunit_input_map[cmd_id] = job_obj

			}

		} else {

			if input.Default == nil {
				err = fmt.Errorf("(GetStepInputObjects) sorry, source and Default are missing") // TODO StepInputExpressionRequirement
				return
			}

			var default_value cwl.CWLType
			default_value, err = cwl.NewCWLType(cmd_id, input.Default)
			if err != nil {
				err = fmt.Errorf("(GetStepInputObjects) NewCWLTypeFromInterface(input.Default) returns: %s", err.Error())
				return
			}

			if default_value == nil {
				err = fmt.Errorf("(GetStepInputObjects) default_value == nil ")
				return
			}

			workunit_input_map[cmd_id] = default_value

		}
		// TODO

	}
	//fmt.Println("(GetStepInputObjects) workunit_input_map after first round:\n")
	//spew.Dump(workunit_input_map)
	// 3. evaluate each ValueFrom field, update results

	for _, input := range workflow_step.In {
		if input.ValueFrom == "" {
			continue
		}

		id := input.Id
		cmd_id := path.Base(id)

		// from CWL doc: The self value of in the parameter reference or expression must be the value of the parameter(s) specified in the source field, or null if there is no source field.

		// #### Create VM ####
		vm := otto.New()

		// set "inputs"

		//func ToValue(value interface{}) (Value, error)

		//var inputs_value otto.Value
		//inputs_value, err = vm.ToValue(workunit_input_map)
		//if err != nil {
		//	return
		//}

		var inputs_json []byte
		inputs_json, err = json.Marshal(workunit_input_map)
		if err != nil {
			err = fmt.Errorf("(GetStepInputObjects) json.Marshal returns: %s", err.Error())
			return
		}
		logger.Debug(3, "SET inputs=%s\n", inputs_json)

		//err = vm.Set("inputs", workunit_input_map)
		//err = vm.Set("inputs_str", inputs_json)
		//if err != nil {
		//	err = fmt.Errorf("(GetStepInputObjects) vm.Set inputs returns: %s", err.Error())
		//	return
		//}

		js_self, ok := workunit_input_map[cmd_id]
		if !ok {
			err = fmt.Errorf("(GetStepInputObjects) workunit_input %s not found", cmd_id)
			return
		}

		if js_self == nil {
			err = fmt.Errorf("(GetStepInputObjects) js_self == nil")
			return
		}

		var self_json []byte
		self_json, err = json.Marshal(js_self)
		if err != nil {
			err = fmt.Errorf("(GetStepInputObjects) json.Marshal returned: %s", err.Error())
			return
		}

		logger.Debug(3, "SET self=%s\n", self_json)

		//err = vm.Set("self", js_self)
		//err = vm.Set("self_str", self_json)
		//if err != nil {
		//	err = fmt.Errorf("(GetStepInputObjects) vm.Set self returns: %s", err.Error())
		//	return
		//}

		//fmt.Printf("input.ValueFrom=%s\n", input.ValueFrom)

		// evaluate $(...) ECMAScript expression
		reg := regexp.MustCompile(`\$\([\w.]+\)`)
		// CWL documentation: http://www.commonwl.org/v1.0/Workflow.html#Expressions

		parsed := input.ValueFrom.String()
		for {

			matches := reg.FindAll([]byte(parsed), -1)
			fmt.Printf("Matches: %d\n", len(matches))
			if len(matches) == 0 {
				break
			}
			for _, match := range matches {
				expression_string := bytes.TrimPrefix(match, []byte("$("))
				expression_string = bytes.TrimSuffix(expression_string, []byte(")"))

				javascript_function := fmt.Sprintf("(function(){\n self=%s ; inputs=%s; return %s;\n})()", self_json, inputs_json, expression_string)
				fmt.Printf("%s\n", javascript_function)

				value, xerr := vm.Run(javascript_function)
				if xerr != nil {
					err = fmt.Errorf("Javascript complained: A) %s", xerr.Error())
					return
				}
				fmt.Println(reflect.TypeOf(value))

				value_str, xerr := value.ToString()
				if xerr != nil {
					err = fmt.Errorf("Cannot convert value to string: %s", xerr.Error())
					return
				}
				parsed = strings.Replace(parsed, string(match), value_str, 1)
			}

		}

		fmt.Printf("parsed: %s\n", parsed)

		// evaluate ${...} ECMAScript function body
		reg = regexp.MustCompile(`(?s)\${.+}`) // s-flag is needed to include newlines

		// CWL documentation: http://www.commonwl.org/v1.0/Workflow.html#Expressions

		matches := reg.FindAll([]byte(parsed), -1)
		fmt.Printf("Matches: %d\n", len(matches))
		if len(matches) == 0 {

			workunit_input_map[cmd_id] = cwl.NewString(parsed)
		} else if len(matches) == 1 {
			match := matches[0]
			expression_string := bytes.TrimPrefix(match, []byte("${"))
			expression_string = bytes.TrimSuffix(expression_string, []byte("}"))

			javascript_function := fmt.Sprintf("(function(){\n self=%s ; inputs=%s; %s \n})()", self_json, inputs_json, expression_string)
			fmt.Printf("%s\n", javascript_function)

			value, xerr := vm.Run(javascript_function)
			if xerr != nil {
				err = fmt.Errorf("Javascript complained: B) %s", xerr.Error())
				return
			}

			value_exported, _ := value.Export()

			fmt.Printf("reflect.TypeOf(value_exported): %s\n", reflect.TypeOf(value_exported))

			var value_cwl cwl.CWLType
			value_cwl, err = cwl.NewCWLType("", value_exported)
			if err != nil {
				err = fmt.Errorf("(NewWorkunit) Error parsing javascript VM result value, cwl.NewCWLType returns: %s", err.Error())
				return
			}

			workunit_input_map[cmd_id] = value_cwl
		} else {
			err = fmt.Errorf("(NewWorkunit) ValueFrom contains more than one ECMAScript function body")
			return
		}

	}
	return
}

func (qm *ServerMgr) CreateAndEnqueueWorkunits(task *Task, job *Job) (err error) {
	logger.Debug(3, "(CreateAndEnqueueWorkunits) starting")
	workunits, err := task.CreateWorkunits(qm, job)
	if err != nil {
		err = fmt.Errorf("(CreateAndEnqueueWorkunits) error in CreateWorkunits: %s", err.Error())
		return err
	}
	for _, wu := range workunits {
		if err := qm.workQueue.Add(wu); err != nil {
			err = fmt.Errorf("(CreateAndEnqueueWorkunits) error in qm.workQueue.Add: %s", err.Error())
			return err
		}
		id := wu.GetId()
		err = qm.CreateWorkPerf(id)
		if err != nil {
			err = fmt.Errorf("(CreateAndEnqueueWorkunits) error in CreateWorkPerf: %s", err.Error())
			return
		}
	}
	return
}

func (qm *ServerMgr) createOutputNode(task *Task) (err error) {
	err = task.LockNamed("createOutputNode")
	if err != nil {
		return
	}
	defer task.Unlock()

	var modified bool
	for _, io := range task.Outputs {
		if io.Type == "update" {
			// this an update output, it will update an existing shock node and not create a new one (it will update metadata of the shock node)
			if (io.Node == "") || (io.Node == "-") {
				if io.Origin == "" {
					// it may be in inputs
					for _, input := range task.Inputs {
						if io.FileName == input.FileName {
							io.Node = input.Node
							io.Size = input.Size
							io.Url = input.Url
							modified = true
						}
					}
					if (io.Node == "") || (io.Node == "-") {
						// still missing
						err = fmt.Errorf("update output %s in task %s is missing required origin", io.FileName, task.Id)
						return
					}
				} else {
					// find predecessor task
					var preId Task_Unique_Identifier
					preId, err = New_Task_Unique_Identifier(task.JobId, "", io.Origin)
					if err != nil {
						err = fmt.Errorf("New_Task_Unique_Identifier returned: %s", err.Error())
						return
					}
					var preTaskStr string
					preTaskStr, err = preId.String()
					if err != nil {
						err = fmt.Errorf("task.String returned: %s", err.Error())
						return
					}
					preTask, ok, xerr := qm.TaskMap.Get(preId, true)
					if xerr != nil {
						err = fmt.Errorf("predecessor task %s not found for task %s: %s", preTaskStr, task.Id, xerr.Error())
						return
					}
					if !ok {
						err = fmt.Errorf("predecessor task %s not found for task %s", preTaskStr, task.Id)
						return
					}

					// find predecessor output
					preTaskIO, xerr := preTask.GetOutput(io.FileName)
					if xerr != nil {
						err = fmt.Errorf("unable to get IO for predecessor task %s, file %s: %s", preTask.Id, io.FileName, err.Error())
						return
					}

					// copy if not already done
					if io.Node != preTaskIO.Node {
						io.Node = preTaskIO.Node
						modified = true
					}
					if io.Size != preTaskIO.Size {
						io.Size = preTaskIO.Size
						modified = true
					}
					if io.Url != preTaskIO.Url {
						io.Url = preTaskIO.Url
						modified = true
					}
				}
				logger.Debug(2, "(createOutputNode) outout %s in task %s is an update of node %s", io.FileName, task.Id, io.Node)
			}
		} else {
			// POST empty shock node for this output
			logger.Debug(2, "(createOutputNode) posting output Shock node for file %s in task %s", io.FileName, task.Id)

			sc := shock.ShockClient{Host: io.Host, Token: task.Info.DataToken}
			nodeid, nerr := sc.PostNodeWithToken(io.FileName, task.TotalWork)
			if nerr != nil {
				err = fmt.Errorf("PostNodeWithToken failed: %s", nerr.Error())
				return
			}
			io.Node = nodeid
			_, err = io.DataUrl()
			if err != nil {
				err = fmt.Errorf("DataUrl failed: %s", err.Error())
				return
			}
			modified = true
			logger.Debug(2, "(createOutputNode) task %s: output Shock node created, node=%s", task.Id, nodeid)
		}
	}

	if modified {
		err = dbUpdateJobTaskIO(task.JobId, task.Id, "outputs", task.Outputs)
		if err != nil {
			err = fmt.Errorf("unable to save task outputs to mongodb, task=%s: %s", task.Id, err.Error())
		}
	}
	return
}

//---end of task methods---

//update job info when a task in that job changed to a new state
// update parent task in a subworkflow
// this is invoked everytime a task changes state, but only when job_remainTasks==0 and state is not yet JOB_STAT_COMPLETED it will complete the job
func (qm *ServerMgr) updateJobTask(task *Task) (err error) {
	//parts := strings.Split(task.Id, "_")
	//jobid := parts[0]
	jobid, err := task.GetJobId()
	if err != nil {
		return
	}
	var job *Job
	job, err = GetJob(jobid)
	if err != nil {
		return
	}

	var task_state string
	task_state, err = task.GetState()
	if err != nil {
		return
	}

	var job_remainTasks int
	job_remainTasks, err = job.GetRemainTasks() // TODO deprecated !?
	if err != nil {
		return
	}

	var task_str string
	task_str, err = task.String()
	if err != nil {
		err = fmt.Errorf("(updateJobTask) task.String returned: %s", err.Error())
		return
	}

	logger.Debug(2, "(updateJobTask) remaining tasks for job %s: %d (task_state: %s)", task_str, job_remainTasks, task_state)

	// check if this was the last task in a subworkflow

	// check if this task has a parent
	var parent_id_str string
	parent_id_str, err = task.GetParent()
	if err != nil {
		return
	}

	if task.WorkflowStep == nil {
		logger.Debug(3, "(updateJobTask) task.WorkflowStep == nil ")
	} else {
		logger.Debug(3, "(updateJobTask) task.WorkflowStep != nil ")
	}

	// CWL Task completes
	if task_state == TASK_STAT_COMPLETED && task.WorkflowStep != nil {
		// this task belongs to a subworkflow // TODO every task should belong to a subworkflow
		logger.Debug(3, "(updateJobTask) task_state == TASK_STAT_COMPLETED && task.WorkflowStep != nil")

		var subworkflow_remain_tasks int
		subworkflow_remain_tasks, err = job.Decrease_WorkflowInstance_RemainTasks(parent_id_str)
		if err != nil {
			//fmt.Printf("ERROR: (updateJobTask) Decrease_WorkflowInstance_RemainTasks returned: %s\n", err.Error())
			err = fmt.Errorf("(updateJobTask) WorkflowInstanceDecreaseRemainTasks returned: %s", err.Error())
			return
		}

		logger.Debug(3, "(updateJobTask) TASK_STAT_COMPLETED  / remaining tasks for subworkflow %s: %d", task_str, subworkflow_remain_tasks)

		if subworkflow_remain_tasks > 0 {
			return
		}

		// subworkflow completed.

		var wfl *cwl.Workflow
		var cwl_step *cwl.WorkflowStep

		var parent_task *Task
		var ok bool

		process_name := job.Entrypoint

		var parent_id Task_Unique_Identifier
		if parent_id_str != "" { // a real explicit subworkflow
			parent_id, err = New_Task_Unique_Identifier_FromString(jobid + "_" + parent_id_str)
			if err != nil {
				err = fmt.Errorf("(updateJobTask) New_Task_Unique_Identifier_FromString returned: %s", err.Error())
				return
			}
			// find parent task

			parent_task, ok, err = qm.TaskMap.Get(parent_id, true)
			if err != nil {
				return
			}
			if !ok {
				err = fmt.Errorf("(updateJobTask) Parent task %s not found", parent_id)
				return
			}

			// double checking
			var parent_task_type string
			parent_task_type, err = parent_task.GetTaskType()
			if err != nil {
				return
			}

			if parent_task_type != TASK_TYPE_WORKFLOW {
				err = fmt.Errorf("(updateJobTask) Uhhh ? Parent task %s has not type workflow???", parent_id_str)
				return
			}

			// get all tasks in the sub-workflow to check if subworkflow has completed
			var children []*Task
			children, err = parent_task.GetChildren(qm)
			if err != nil {
				return
			}

			subworkflow_complete := true
			for _, child_task := range children {
				var child_state string
				child_state, err = child_task.GetState()
				if err != nil {
					return
				}

				if child_state != TASK_STAT_COMPLETED {
					subworkflow_complete = false
					break
				}
			}

			if !subworkflow_complete {
				// nothing to do here, sub-workflow is not complete
				return
			}

			// ****************************************************
			// **** sub workflow is complete, collect outputs *****
			// ****************************************************

			// this prevents a race condition, in case multiple tasks in the same subworkflow complete at the same time
			ok, err = parent_task.Finalize()
			if err != nil {
				return
			}

			if !ok {
				// somebody else is finalizing
				return
			}

			// Copy outputs (populate parent_task.StepOutput)

			cwl_step = parent_task.WorkflowStep

			p := cwl_step.Run

			if p == nil {
				err = fmt.Errorf("(updateJobTask) process is nil !?")
				return
			}
			// check if this is a workflow

			var schemata []cwl.CWLType_Type
			schemata, err = job.CWL_collection.GetSchemata()
			if err != nil {
				err = fmt.Errorf("(updateJobTask) job.CWL_collection.GetSchemata() returned: %s", err.Error())
				return
			}
			//var a_workflow *cwl.Workflow
			//process_name, _, a_workflow, _, _, err = cwl.GetProcess(p, job.CWL_collection, schemata)
			//if err != nil {
			//	err = fmt.Errorf("(updateJobTask) embedded workflow or toll not supported yet: %s", err.Error())
			//	return
			//}

			var process interface{}
			process, _, err = cwl.GetProcess(p, job.CWL_collection, job.CwlVersion, schemata) // TODO add schemata

			// get embedded workflow
			var ok bool
			wfl, ok = process.(*cwl.Workflow)
			if !ok {
				wfl = nil
			}

		} else {

			// the job completes !

			// implicit subworkflow (there is no parent)
			wfl, err = job.CWL_collection.GetWorkflow(job.Entrypoint) // TODO: use locked function
			if err != nil {
				// not a workflow
				err = fmt.Errorf("(updateJobTask) %s is not a workflow ????", job.Entrypoint)
				return
			}

		}

		var task_id Task_Unique_Identifier
		task_id, err = task.GetId("updateJobTask")
		if err != nil {
			return
		}

		var workflow_instance *WorkflowInstance
		workflow_instance, err = job.GetWorkflowInstance(parent_id_str, true)
		if err != nil {
			return
		}
		workflow_inputs := workflow_instance.Inputs

		workflow_inputs_map := workflow_inputs.GetMap()

		workflow_outputs_map := make(cwl.JobDocMap)

		// collect sub-workflow outputs, put results in workflow_outputs_map

		for _, output := range wfl.Outputs { // WorkflowOutputParameter http://www.commonwl.org/v1.0/Workflow.html#WorkflowOutputParameter

			output_id := output.Id

			if output.OutputBinding != nil {
				// see http://www.commonwl.org/v1.0/Workflow.html#CommandOutputBinding
				//spew.Dump(output.OutputBinding)
				// import path
				// use https://golang.org/pkg/path/#Match
				// iterate over output files
				for _, value := range workflow_inputs_map {
					//fmt.Println("key: " + key)

					_, ok := value.(*cwl.File)
					if !ok {
						continue
					}

					//fmt.Println("base: " + file.Basename)

				}

				//panic("ok")
				err = fmt.Errorf("(updateJobTask) Workflow output outputbinding not supported yet")
				return
			}

			expected_types_raw := output.Type

			expected_types := []cwl.CWLType_Type{}

			is_optional := false

			var schemata []cwl.CWLType_Type
			schemata, err = job.CWL_collection.GetSchemata()
			if err != nil {
				err = fmt.Errorf("(updateJobTask) job.CWL_collection.GetSchemata returned: %s", err.Error())
				return
			}

			for _, raw_type := range expected_types_raw {
				var type_correct cwl.CWLType_Type
				type_correct, err = cwl.NewCWLType_Type(schemata, raw_type, "WorkflowOutput")
				if err != nil {
					spew.Dump(expected_types_raw)
					fmt.Println("---")
					spew.Dump(raw_type)

					err = fmt.Errorf("(updateJobTask) could not convert element of output.Type into cwl.CWLType_Type: %s", err.Error())
					//fmt.Printf(err.Error())
					//panic("raw_type problem")
					return
				}
				expected_types = append(expected_types, type_correct)
				if type_correct == cwl.CWL_null {
					is_optional = true
				}
			}

			//XXXXXXXX

			output_source := output.OutputSource

			switch output_source.(type) {
			case string:
				outputSourceString := output_source.(string)
				// example: "#preprocess-fastq.workflow.cwl/rejected2fasta/file"

				var obj cwl.CWLType
				var ok bool
				obj, ok, err = qm.getCWLSource(workflow_inputs_map, job, task_id, outputSourceString, true)
				if err != nil {
					err = fmt.Errorf("(updateJobTask) A getCWLSource returns: %s", err.Error())
					return
				}
				skip := false
				if !ok {
					if is_optional {
						skip = true
					} else {
						err = fmt.Errorf("(updateJobTask) A source %s not found", outputSourceString)
						return
					}
				}

				if !skip {
					has_type, xerr := cwl.TypeIsCorrect(expected_types, obj)
					if xerr != nil {
						err = fmt.Errorf("(updateJobTask) TypeIsCorrect: %s", xerr.Error())
						return
					}
					if !has_type {
						err = fmt.Errorf("(updateJobTask) A) workflow_ouput %s (type: %s), does not match expected types %s", output_id, reflect.TypeOf(obj), expected_types)
						return
					}

					workflow_outputs_map[output_id] = obj
				}
			case []string:
				outputSourceArrayOfString := output_source.([]string)

				if len(outputSourceArrayOfString) == 0 {
					if !is_optional {
						err = fmt.Errorf("(updateJobTask) output_source array (%s) is empty, but a required output", output_id)
						return
					}
				}

				output_array := cwl.Array{}

				for _, outputSourceString := range outputSourceArrayOfString {
					var obj cwl.CWLType
					var ok bool
					obj, ok, err = qm.getCWLSource(workflow_inputs_map, job, task_id, outputSourceString, true)
					if err != nil {
						err = fmt.Errorf("(updateJobTask) B (%s) getCWLSource returns: %s", parent_id_str, err.Error())
						return
					}

					skip := false
					if !ok {

						if is_optional {
							skip = true
						} else {

							err = fmt.Errorf("(updateJobTask) B (%s) source %s not found", parent_id_str, outputSourceString)
							return
						}
					}

					if !skip {
						has_type, xerr := cwl.TypeIsCorrect(expected_types, obj)
						if xerr != nil {
							err = fmt.Errorf("(updateJobTask) TypeIsCorrect: %s", xerr.Error())
							return
						}
						if !has_type {
							err = fmt.Errorf("(updateJobTask) B) workflow_ouput %s, does not match expected types %s", output_id, expected_types)
							return
						}
						fmt.Println("obj:")
						spew.Dump(obj)
						output_array = append(output_array, obj)
					}
				}

				if len(output_array) > 0 {
					workflow_outputs_map[output_id] = &output_array
				} else {
					if !is_optional {
						err = fmt.Errorf("(updateJobTask) array with output_id %s is empty, but a required output", output_id)
						return
					}
				}
				fmt.Println("workflow_outputs_map:")
				spew.Dump(workflow_outputs_map)

			default:
				err = fmt.Errorf("(updateJobTask) output.OutputSource has to be string or []string, but I got type %s", spew.Sdump(output_source))
				return

			}

		}

		//fmt.Println("(updateJobTask) workflow_outputs_map:")
		//spew.Dump(workflow_outputs_map)
		step_outputs := cwl.Job_document{}

		if parent_id_str != "" {
			// save outputs in workflow_instance
			// step_outputs is a subset of the subworkflow outputs

			if cwl_step == nil {
				err = fmt.Errorf("(updateJobTask) step == nil")
				return
			}

			// get step outputs from subworkflow outputs
			for _, output := range cwl_step.Out { // output is a WorkflowStepOutput
				//fmt.Println("output: " + output.Id)
				output_base := path.Base(output.Id)

				real_name := process_name + "/" + output_base
				//fmt.Println("(updateJobTask) output real: " + real_name)

				var object cwl.CWLType
				object, ok = workflow_outputs_map[real_name]
				if !ok {

					//out_list := ""
					//for key := range workflow_outputs_map {
					//	out_list += "," + key
					//}

					//err = fmt.Errorf("(updateJobTask) workflow output %s not found (available: %s)", real_name, out_list)
					//return
					object = cwl.NewNull()
				}

				named_obj := cwl.NewNamedCWLType(output_base, object)
				step_outputs = append(step_outputs, named_obj)

			}
		} else {
			// the main workflow, a implicit subworkflow
			// get outputs from subworkflow outputs
			for _, output := range wfl.Outputs { // output is a WorkflowOutputParameter
				//fmt.Println("output: " + output.Id)
				output_base := path.Base(output.Id)

				real_name := process_name + "/" + output_base
				//fmt.Println("(updateJobTask) output real: " + real_name)

				var object cwl.CWLType
				object, ok = workflow_outputs_map[real_name]
				if !ok {
					//out_list := ""
					//for key := range workflow_outputs_map {
					//	out_list += "," + key
					//}
					//err = fmt.Errorf("(updateJobTask) workflow output %s not found (available: %s)", real_name, out_list)
					//return

					object = cwl.NewNull()
				}

				named_obj := cwl.NewNamedCWLType(output_base, object)
				step_outputs = append(step_outputs, named_obj)

			}

		}
		var workflow_outputs_array cwl.Job_document
		workflow_outputs_array, err = workflow_outputs_map.GetArray()
		if err != nil {
			err = fmt.Errorf("(updateJobTask) workflow_outputs_map.GetArray returned: %s", err.Error())
			return
		}

		//fmt.Println("wfl.Outputs:")
		//spew.Dump(wfl.Outputs)
		//fmt.Println("workflow_outputs_map:")
		//spew.Dump(workflow_outputs_map)
		//fmt.Println("workflow_outputs_array:")
		//spew.Dump(workflow_outputs_array)

		//panic("xxxxxxx")

		//workflow_instance.Outputs = step_outputs
		err = job.Set_WorkflowInstance_Outputs(parent_id_str, workflow_outputs_array)
		if err != nil {
			return
		}

		if parent_id_str != "" {
			// ##### Step Output #####
			err = parent_task.SetStepOutput(&workflow_outputs_array, true)
			if err != nil {

				return err
			}

			err = parent_task.SetState(TASK_STAT_COMPLETED, true)
			if err != nil {
				return
			}
		} else {

			if job_remainTasks > 0 {
				err = fmt.Errorf("(updateJobTask) Something is wrong, last subworkflow completes, but job_remainTasks > 0 , does not make sense")
				return
			}

		}
	}

	if job_remainTasks > 0 { //#####################################################################
		return
	}

	job_state, err := job.GetState(true)
	if err != nil {
		return
	}
	if job_state == JOB_STAT_COMPLETED {
		err = fmt.Errorf("(updateJobTask) job state is already JOB_STAT_COMPLETED")
		return
	}

	err = job.SetState(JOB_STAT_COMPLETED, nil)
	if err != nil {
		return
	}

	qm.FinalizeJobPerf(jobid)
	qm.LogJobPerf(jobid)
	qm.removeActJob(jobid)
	//delete tasks in task map
	//delete from shock output flagged for deletion

	modified := 0
	for i, task := range job.TaskList() {
		// delete nodes that have been flagged to be deleted
		modified += task.DeleteOutput()
		modified += task.DeleteInput()
		//combined_id := jobid + "_" + task.Id

		id, _ := task.GetId("updateJobTask." + strconv.Itoa(i))

		qm.TaskMap.Delete(id)
	}

	if modified > 0 {
		// save only is something has changed
		job.Save() // TODO avoid this, try partial updates
	}

	//set expiration from conf if not set
	nullTime := time.Time{}

	job_expiration, xerr := dbGetJobFieldTime(jobid, "expiration")
	if xerr != nil {
		err = xerr
		return
	}

	if job_expiration == nullTime {
		expire := conf.GLOBAL_EXPIRE

		job_info_pipeline, xerr := dbGetJobFieldString(jobid, "info.pipeline")
		if xerr != nil {
			err = xerr
			return
		}

		if val, ok := conf.PIPELINE_EXPIRE_MAP[job_info_pipeline]; ok {
			expire = val
		}
		if expire != "" {
			if err := job.SetExpiration(expire); err != nil {
				return err
			}
		}
	}
	//log event about job done (JD)
	logger.Event(event.JOB_DONE, "jobid="+job.Id+";name="+job.Info.Name+";project="+job.Info.Project+";user="+job.Info.User)

	return
}

//update job/task states from "queued" to "in-progress" once the first workunit is checked out
func (qm *ServerMgr) UpdateJobTaskToInProgress(works []*Workunit) (err error) {
	for _, work := range works {
		//job_was_inprogress := false
		//task_was_inprogress := false
		taskid := work.GetTask()
		jobid := work.JobId

		// get job state
		job, xerr := GetJob(jobid)
		if xerr != nil {
			err = xerr
			return
		}

		job_state, xerr := job.GetState(true)
		if xerr != nil {
			err = xerr
			return
		}

		//update job status
		if job_state != JOB_STAT_INPROGRESS {
			err = job.SetState(JOB_STAT_INPROGRESS, nil)
			if err != nil {
				return
			}
			qm.UpdateJobPerfStartTime(jobid)
		}

		task, ok, err := qm.TaskMap.Get(taskid, true)
		if err != nil {
			logger.Error("(UpdateJobTaskToInProgress) %s", err.Error())
			continue
		}
		if !ok {
			logger.Error("(UpdateJobTaskToInProgress) task %s not found", taskid)
			continue
		}

		task_state, err := task.GetState()
		if err != nil {
			logger.Error("(UpdateJobTaskToInProgress) dbGetJobTaskField: %s", err.Error())
			continue
		}

		if task_state != TASK_STAT_INPROGRESS {
			err := task.SetState(TASK_STAT_INPROGRESS, true)
			if err != nil {
				logger.Error("(UpdateJobTaskToInProgress) could not update task %s", taskid)
				continue
			}
			err = qm.UpdateTaskPerfStartTime(task)
			if err != nil {
				logger.Error("(UpdateJobTaskToInProgress) UpdateTaskPerfStartTime: %s", err.Error())
				continue
			}

		}
	}
	return
}

func (qm *ServerMgr) IsJobRegistered(id string) bool {
	if qm.isActJob(id) {
		return true
	}
	if qm.isSusJob(id) {
		return true
	}
	return false
}

// use for JOB_STAT_SUSPEND and JOB_STAT_FAILED_PERMANENT
func (qm *ServerMgr) SuspendJob(jobid string, jerror *JobError) (err error) {
	job, err := GetJob(jobid)
	if err != nil {
		return
	}

	err = job.SetState(jerror.Status, nil)
	if err != nil {
		return
	}

	// set error struct
	err = job.SetError(jerror)
	if err != nil {
		return
	}

	//suspend queueing workunits
	var workunit_list []*Workunit
	workunit_list, err = qm.workQueue.GetAll()
	if err != nil {
		return
	}

	new_work_state := WORK_STAT_SUSPEND
	new_task_state := TASK_STAT_SUSPEND
	this_event := event.JOB_SUSPEND
	if jerror.Status == JOB_STAT_FAILED_PERMANENT {
		new_work_state = WORK_STAT_FAILED_PERMANENT
		new_task_state = TASK_STAT_FAILED_PERMANENT
		this_event = event.JOB_FAILED_PERMANENT
	}

	// update all workunits
	for _, workunit := range workunit_list {
		workid := workunit.Workunit_Unique_Identifier
		parentid := workunit.JobId
		//parentid, _ := GetJobIdByWorkId(workid)
		if jobid == parentid {
			qm.workQueue.StatusChange(workid, nil, new_work_state, "see job error")
		}
	}

	//suspend parsed tasks
	for _, task := range job.Tasks {
		var task_state string
		task_state, err = task.GetState()
		if err != nil {
			continue
		}
		if task_state == TASK_STAT_QUEUED || task_state == TASK_STAT_READY || task_state == TASK_STAT_INPROGRESS {
			err = task.SetState(new_task_state, true)
			if err != nil {
				logger.Error("(SuspendJob) : %s", err.Error())
				continue
			}
		}
	}
	qm.LogJobPerf(jobid)
	qm.removeActJob(jobid)

	// log event and reason
	var reason string
	if jerror.ServerNotes != "" {
		reason = jerror.ServerNotes
	} else if jerror.WorkNotes != "" {
		reason = jerror.WorkNotes
	}
	logger.Event(this_event, "jobid="+jobid+";reason="+reason)
	return
}

func (qm *ServerMgr) DeleteJobByUser(jobid string, u *user.User, full bool) (err error) {
	var job *Job
	job, err = GetJob(jobid)
	if err != nil {
		return
	}
	// User must have delete permissions on job or be job owner or be an admin
	rights := job.Acl.Check(u.Uuid)
	if job.Acl.Owner != u.Uuid && rights["delete"] == false && u.Admin == false {
		return errors.New(e.UnAuth)
	}
	if err = job.SetState(JOB_STAT_DELETED, nil); err != nil {
		return
	}
	//delete queueing workunits
	var workunit_list []*Workunit
	workunit_list, err = qm.workQueue.GetAll()
	if err != nil {
		return
	}
	for _, workunit := range workunit_list {
		workid := workunit.Workunit_Unique_Identifier
		workunit_jobid := workid.JobId
		//parentid, _ := GetJobIdByWorkId(workid)
		if jobid == workunit_jobid {
			qm.workQueue.Delete(workid)
		}
	}
	//delete parsed tasks
	for i := 0; i < len(job.TaskList()); i++ {
		//task_id := fmt.Sprintf("%s_%d", jobid, i)
		var task_id Task_Unique_Identifier
		task_id, err = New_Task_Unique_Identifier(jobid, "", strconv.Itoa(i)) // TODO that will not work
		if err != nil {
			return
		}
		qm.TaskMap.Delete(task_id)
	}
	qm.removeActJob(jobid)
	//qm.removeSusJob(jobid)
	// delete from job map
	if err = JM.Delete(jobid, true); err != nil {
		return
	}
	// really delete it !
	if full {
		return job.Delete()
	} else {
		logger.Event(event.JOB_DELETED, "jobid="+jobid)
	}
	return
}

func (qm *ServerMgr) DeleteSuspendedJobsByUser(u *user.User, full bool) (num int) {
	for id := range qm.GetSuspendJobs() {
		if err := qm.DeleteJobByUser(id, u, full); err == nil {
			num += 1
		}
	}
	return
}

func (qm *ServerMgr) ResumeSuspendedJobsByUser(u *user.User) (num int) {
	for id := range qm.GetSuspendJobs() {
		if err := qm.ResumeSuspendedJobByUser(id, u); err == nil {
			num += 1
		}
	}
	return
}

//delete jobs in db with "queued" or "in-progress" state but not in the queue (zombie jobs) that user has access to
func (qm *ServerMgr) DeleteZombieJobsByUser(u *user.User, full bool) (num int) {
	dbjobs := new(Jobs)
	q := bson.M{}
	q["state"] = bson.M{"in": JOB_STATS_ACTIVE}
	if err := dbjobs.GetAll(q, "info.submittime", "asc", false); err != nil {
		logger.Error("DeleteZombieJobs()->GetAllLimitOffset():" + err.Error())
		return
	}
	for _, dbjob := range *dbjobs {
		if !qm.isActJob(dbjob.Id) {
			if err := qm.DeleteJobByUser(dbjob.Id, u, full); err == nil {
				num += 1
			}
		}
	}
	return
}

//resubmit a suspended job if the user is authorized
func (qm *ServerMgr) ResumeSuspendedJobByUser(id string, u *user.User) (err error) {
	//Load job by id
	dbjob, err := GetJob(id)
	if err != nil {
		err = errors.New("(ResumeSuspendedJobByUser) failed to load job " + err.Error())
		return
	}

	job_state, err := dbjob.GetState(true)
	if err != nil {
		err = errors.New("(ResumeSuspendedJobByUser) failed to get job state " + err.Error())
		return
	}

	// User must have write permissions on job or be job owner or be an admin
	rights := dbjob.Acl.Check(u.Uuid)
	if dbjob.Acl.Owner != u.Uuid && rights["write"] == false && u.Admin == false {
		err = errors.New(e.UnAuth)
		return
	}

	if job_state != JOB_STAT_SUSPEND {
		err = errors.New("(ResumeSuspendedJobByUser) job " + id + " is not in 'suspend' status")
		return
	}
	logger.Debug(1, "resumeing job=%s, state=%s", id, job_state)

	tasks, err := dbjob.GetTasks()
	if err != nil {
		err = errors.New("(ResumeSuspendedJobByUser) failed to get job tasks " + err.Error())
		return
	}

	for _, task := range tasks {
		task_state, serr := task.GetState()
		if serr != nil {
			err = errors.New("(ResumeSuspendedJobByUser) failed to get task state " + serr.Error())
			return
		}
		if contains(TASK_STATS_RESET, task_state) {
			logger.Debug(1, "(ResumeSuspendedJobByUser/ResetTaskTrue) task=%s, state=%s", task.Id, task_state)
			err = task.ResetTaskTrue("Resume")
			if err != nil {
				err = errors.New("(ResumeSuspendedJobByUser) failed to reset task " + err.Error())
				return
			}
		}
	}

	err = dbjob.IncrementResumed(1)
	if err != nil {
		err = errors.New("(ResumeSuspendedJobByUser) failed to incremenet job resumed " + err.Error())
		return
	}

	err = dbjob.SetState(JOB_STAT_QUEUING, nil)
	if err != nil {
		err = fmt.Errorf("(ResumeSuspendedJobByUser) UpdateJobState: %s", err.Error())
		return
	}
	err = qm.EnqueueTasksByJobId(id)
	if err != nil {
		err = errors.New("(ResumeSuspendedJobByUser) failed to enqueue job " + err.Error())
		return
	}
	logger.Debug(1, "Resumed job %s", id)
	return
}

//recover a job in db that is missing from queue (caused by server restarting)
func (qm *ServerMgr) RecoverJob(id string, job *Job) (recovered bool, err error) {
	// job by id or object
	if job != nil {
		id = job.Id
	} else {
		job, err = GetJob(id)
		if err != nil {
			err = errors.New("(RecoverJob) failed to load job " + err.Error())
			return
		}
	}

	if qm.isActJob(id) {
		// already acive, skip
		return
	}

	job_state, err := job.GetState(true)
	if err != nil {
		err = errors.New("(RecoverJob) failed to get job state " + err.Error())
		return
	}

	if job_state == JOB_STAT_SUSPEND {
		// just add suspended jobs to in-memory map
		err = JM.Add(job)
		if err != nil {
			err = errors.New("(RecoverJob) JM.Add failed " + err.Error())
			return
		}
	} else {
		if job_state == JOB_STAT_COMPLETED || job_state == JOB_STAT_DELETED || job_state == JOB_STAT_FAILED_PERMANENT {
			// unrecoverable, skip
			return
		}
		tasks, terr := job.GetTasks()
		if terr != nil {
			err = errors.New("(RecoverJob) failed to get job tasks " + terr.Error())
			return
		}
		for _, task := range tasks {
			task_state, serr := task.GetState()
			if serr != nil {
				err = errors.New("(RecoverJob) failed to get task state " + serr.Error())
				return
			}
			if contains(TASK_STATS_RESET, task_state) {
				logger.Debug(1, "(RecoverJob/ResetTaskTrue) task=%s, state=%s", task.Id, task_state)
				err = task.ResetTaskTrue("Recover")
				if err != nil {
					err = errors.New("(RecoverJob) failed to reset task " + err.Error())
					return
				}
			}
		}
		err = job.SetState(JOB_STAT_QUEUING, nil)
		if err != nil {
			err = errors.New("(RecoverJob) UpdateJobState: " + err.Error())
			return
		}
		err = qm.EnqueueTasksByJobId(id)
		if err != nil {
			err = errors.New("(RecoverJob) failed to enqueue job: " + err.Error())
			return
		}
	}
	recovered = true
	logger.Debug(1, "(RecoverJob) done job=%s", id)
	return
}

//recover jobs not completed before awe-server restarts
func (qm *ServerMgr) RecoverJobs() (recovered int, total int, err error) {
	//Get jobs to be recovered from db whose states are recoverable
	dbjobs := new(Jobs)
	q := bson.M{}
	q["state"] = bson.M{"$in": JOB_STATS_TO_RECOVER}
	if conf.RECOVER_MAX > 0 {
		logger.Info("Recover %d jobs...", conf.RECOVER_MAX)
		if _, err = dbjobs.GetPaginated(q, conf.RECOVER_MAX, 0, "info.priority", "desc", true); err != nil {
			logger.Error("(RecoverJobs) (GetPaginated) " + err.Error())
			return
		}
	} else {
		logger.Info("Recover all jobs")
		if err = dbjobs.GetAll(q, "info.submittime", "asc", true); err != nil {
			logger.Error("(RecoverJobs) (GetAll) " + err.Error())
			return
		}
	}
	total = dbjobs.Length()
	//Locate the job script and parse tasks for each job
	for _, dbjob := range *dbjobs {
		pipeline := "missing"
		if dbjob.Info != nil {
			pipeline = dbjob.Info.Pipeline
		}
		logger.Debug(1, "recovering %d: job=%s, state=%s, pipeline=%s", recovered+1, dbjob.Id, dbjob.State, pipeline)
		isRecovered, rerr := qm.RecoverJob("", dbjob)
		if rerr != nil {
			logger.Error(fmt.Sprintf("(RecoverJobs) job=%s failed: %s", dbjob.Id, rerr.Error()))
			continue
		}
		if isRecovered {
			recovered += 1
		}
	}
	return
}

//recompute job from specified task stage
func (qm *ServerMgr) RecomputeJob(jobid string, stage string) (err error) {
	if qm.isActJob(jobid) {
		err = errors.New("(RecomputeJob) job " + jobid + " is already active")
		return
	}
	//Load job by id
	dbjob, err := GetJob(jobid)
	if err != nil {
		err = errors.New("(RecomputeJob) failed to load job " + err.Error())
		return
	}

	job_state, err := dbjob.GetState(true)
	if err != nil {
		err = errors.New("(RecomputeJob) failed to get job state " + err.Error())
		return
	}

	if job_state != JOB_STAT_COMPLETED && job_state != JOB_STAT_SUSPEND {
		err = errors.New("(RecomputeJob) job " + jobid + " is not in 'completed' or 'suspend' status")
		return
	}
	logger.Debug(1, "recomputing: job=%s, state=%s", jobid, job_state)

	from_task_id := fmt.Sprintf("%s_%s", jobid, stage)
	remaintasks := 0
	found := false

	tasks, err := dbjob.GetTasks()
	if err != nil {
		err = errors.New("(RecomputeJob) failed to get job tasks " + err.Error())
		return
	}

	for _, task := range tasks {
		task_str, terr := task.String()
		if terr != nil {
			err = errors.New("(RecomputeJob) failed to get task ID string " + terr.Error())
			return
		}
		if task_str == from_task_id {
			logger.Debug(1, "(RecomputeJob/ResetTaskTrue) task=%s, state=%s", task_str, task.State)
			err = task.ResetTaskTrue("Recompute")
			if err != nil {
				err = errors.New("(RecomputeJob) failed to reset task " + err.Error())
				return
			}
			found = true
			remaintasks += 1
		}
	}
	if !found {
		return errors.New("(RecomputeJob) task not found: " + from_task_id)
	}

	for _, task := range tasks {
		task_str, terr := task.String()
		if terr != nil {
			err = errors.New("(RecomputeJob) failed to get task ID string " + terr.Error())
			return
		}
		is_ancest, aerr := isAncestor(dbjob, task_str, from_task_id)
		if aerr != nil {
			err = errors.New("(RecomputeJob) failed to determine if task is ancestor " + aerr.Error())
			return
		}
		task_state, serr := task.GetState()
		if serr != nil {
			err = errors.New("(RecomputeJob) failed to get task state " + serr.Error())
			return
		}
		if is_ancest || contains(TASK_STATS_RESET, task_state) {
			logger.Debug(1, "(RecomputeJob/ResetTaskTrue) task=%s, state=%s", task_str, task_state)
			err = task.ResetTaskTrue("Recompute")
			if err != nil {
				err = errors.New("(RecomputeJob) failed to reset task " + err.Error())
				return
			}
			remaintasks += 1
		}
	}

	err = dbjob.IncrementResumed(1)
	if err != nil {
		err = errors.New("(RecomputeJob) failed to incremenet job resumed " + err.Error())
		return
	}
	err = dbjob.SetRemainTasks(remaintasks)
	if err != nil {
		err = errors.New("(RecomputeJob) failed to set job remain_tasks " + err.Error())
		return
	}

	err = dbjob.SetState(JOB_STAT_QUEUING, nil)
	if err != nil {
		err = fmt.Errorf("(RecomputeJob) UpdateJobState: %s", err.Error())
		return
	}
	err = qm.EnqueueTasksByJobId(jobid)
	if err != nil {
		err = errors.New("(RecomputeJob) failed to enqueue job " + err.Error())
		return
	}
	logger.Debug(1, "Recomputed job %s from task %d", jobid, stage)
	return
}

//recompute job from beginning
func (qm *ServerMgr) ResubmitJob(jobid string) (err error) {
	if qm.isActJob(jobid) {
		err = errors.New("(ResubmitJob) job " + jobid + " is already active")
		return
	}
	//Load job by id
	job, err := GetJob(jobid)
	if err != nil {
		err = errors.New("(ResubmitJob) failed to load job " + err.Error())
		return
	}

	job_state, err := job.GetState(true)
	if err != nil {
		err = errors.New("(ResubmitJob) failed to get job state " + err.Error())
		return
	}

	if job_state != JOB_STAT_COMPLETED && job_state != JOB_STAT_SUSPEND {
		err = errors.New("(ResubmitJob) job " + jobid + " is not in 'completed' or 'suspend' status")
		return
	}
	logger.Debug(1, "resubmitting: job=%s, state=%s", jobid, job_state)

	remaintasks := 0
	tasks, err := job.GetTasks()
	if err != nil {
		err = errors.New("(ResubmitJob) failed to get job tasks " + err.Error())
		return
	}

	for _, task := range tasks {
		logger.Debug(1, "(ResubmitJob/ResetTaskTrue) task=%s, state=%s", task.Id, task.State)
		err = task.ResetTaskTrue("Resubmit")
		if err != nil {
			err = errors.New("(ResubmitJob) failed to reset task " + err.Error())
			return
		}
		remaintasks += 1
	}

	err = job.IncrementResumed(1)
	if err != nil {
		err = errors.New("(ResubmitJob) failed to incremenet job resumed " + err.Error())
		return
	}
	err = job.SetRemainTasks(remaintasks)
	if err != nil {
		err = errors.New("(ResubmitJob) failed to set job remain_tasks " + err.Error())
		return
	}

	err = job.SetState(JOB_STAT_QUEUING, nil)
	if err != nil {
		err = fmt.Errorf("(ResubmitJob) UpdateJobState: %s", err.Error())
		return
	}
	err = qm.EnqueueTasksByJobId(jobid)
	if err != nil {
		err = errors.New("(ResubmitJob) failed to enqueue job " + err.Error())
		return
	}
	logger.Debug(1, "Restarted job %s from beginning", jobid)
	return
}

func isAncestor(job *Job, taskId string, testId string) (result bool, err error) {
	if taskId == testId {
		result = false
		return
	}
	idx := -1
	for i, task := range job.Tasks {
		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("(isAncestor) task.String returned: %s", err.Error())
			return
		}
		if task_str == taskId {
			idx = i
			break
		}
	}
	if idx == -1 {
		result = false
		return
	}

	task := job.Tasks[idx]
	if len(task.DependsOn) == 0 {
		result = false
		return
	}
	if contains(task.DependsOn, testId) {
		result = true
		return
	} else {
		for _, t := range task.DependsOn {
			return isAncestor(job, t, testId)
		}
	}
	result = false
	return
}

//update tokens for in-memory data structures
func (qm *ServerMgr) UpdateQueueToken(job *Job) (err error) {
	//job_id := job.Id
	for _, task := range job.Tasks {
		task_id, _ := task.GetId("UpdateQueueToken")
		mtask, ok, err := qm.TaskMap.Get(task_id, true)
		if err != nil {
			return err
		}
		if ok {
			mtask.setTokenForIO(true)
		}
	}
	return
}

//---end of job methods

//---perf related methods
func (qm *ServerMgr) CreateJobPerf(jobid string) {
	if !qm.isActJob(jobid) {
		qm.putActJob(NewJobPerf(jobid))
	}
}

func (qm *ServerMgr) UpdateJobPerfStartTime(jobid string) {
	if perf, ok := qm.getActJob(jobid); ok {
		now := time.Now().Unix()
		perf.Start = now
		qm.putActJob(perf)
	}
	return
}

func (qm *ServerMgr) FinalizeJobPerf(jobid string) {
	if perf, ok := qm.getActJob(jobid); ok {
		now := time.Now().Unix()
		perf.End = now
		perf.Resp = now - perf.Queued
		qm.putActJob(perf)
	}
	return
}

func (qm *ServerMgr) CreateTaskPerf(task *Task) (err error) {
	jobid := task.JobId
	//taskid := task.String()
	if perf, ok := qm.getActJob(jobid); ok {
		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("() task.String returned: %s", err.Error())
			return
		}
		perf.Ptasks[task_str] = NewTaskPerf(task_str)
		qm.putActJob(perf)
	}
	return
}

func (qm *ServerMgr) UpdateTaskPerfStartTime(task *Task) (err error) {
	jobid := task.JobId

	if jobperf, ok := qm.getActJob(jobid); ok {
		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("() task.String returned: %s", err.Error())
			return
		}
		if taskperf, ok := jobperf.Ptasks[task_str]; ok {
			now := time.Now().Unix()
			taskperf.Start = now
			qm.putActJob(jobperf)
		}
	}
	return
}

// TODO evaluate err
func (qm *ServerMgr) FinalizeTaskPerf(task *Task) (err error) {
	//jobid, _ := GetJobIdByTaskId(task.Id)
	jobid, err := task.GetJobId()
	if err != nil {
		return
	}
	if jobperf, ok := qm.getActJob(jobid); ok {
		//combined_id := task.String()
		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("() task.String returned: %s", err.Error())
			return
		}

		if taskperf, ok := jobperf.Ptasks[task_str]; ok {
			now := time.Now().Unix()
			taskperf.End = now
			taskperf.Resp = now - taskperf.Queued

			for _, io := range task.Inputs {
				taskperf.InFileSizes = append(taskperf.InFileSizes, io.Size)
			}
			for _, io := range task.Outputs {
				taskperf.OutFileSizes = append(taskperf.OutFileSizes, io.Size)
			}
			qm.putActJob(jobperf)
			return
		}
	}
	return
}

func (qm *ServerMgr) CreateWorkPerf(id Workunit_Unique_Identifier) (err error) {
	if !conf.PERF_LOG_WORKUNIT {
		return
	}
	//workid := id.String()
	jobid := id.JobId
	jobperf, ok := qm.getActJob(jobid)
	if !ok {
		err = fmt.Errorf("(CreateWorkPerf) job perf not found: %s", jobid)
		return
	}
	var work_str string
	work_str, err = id.String()
	if err != nil {
		err = fmt.Errorf("(CreateWorkPerf) id.String() returned: %s", err.Error())
		return
	}
	jobperf.Pworks[work_str] = NewWorkPerf()
	fmt.Println("write jobperf.Pworks: " + work_str)
	qm.putActJob(jobperf)

	return
}

func (qm *ServerMgr) FinalizeWorkPerf(id Workunit_Unique_Identifier, reportfile string) (err error) {
	if !conf.PERF_LOG_WORKUNIT {
		return
	}
	workperf := new(WorkPerf)
	jsonstream, err := ioutil.ReadFile(reportfile)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(jsonstream, workperf); err != nil {
		return err
	}
	jobid := id.JobId
	jobperf, ok := qm.getActJob(jobid)
	if !ok {
		return errors.New("(FinalizeWorkPerf) job perf not found:" + jobid)
	}
	//workid := id.String()
	var work_str string
	work_str, err = id.String()
	if err != nil {
		err = fmt.Errorf("(FinalizeWorkPerf) workid.String() returned: %s", err.Error())
		return
	}
	if _, ok := jobperf.Pworks[work_str]; !ok {
		for key, _ := range jobperf.Pworks {
			fmt.Println("FinalizeWorkPerf jobperf.Pworks: " + key)
		}
		return errors.New("(FinalizeWorkPerf) work perf not found:" + work_str)
	}

	workperf.Queued = jobperf.Pworks[work_str].Queued
	workperf.Done = time.Now().Unix()
	workperf.Resp = workperf.Done - workperf.Queued
	jobperf.Pworks[work_str] = workperf
	qm.putActJob(jobperf)
	os.Remove(reportfile)
	return
}

func (qm *ServerMgr) LogJobPerf(jobid string) {
	if perf, ok := qm.getActJob(jobid); ok {
		perfstr, _ := json.Marshal(perf)
		logger.Perf(string(perfstr)) //write into perf log
		dbUpsert(perf)               //write into mongodb
	}
}

//---end of perf related methods

func (qm *ServerMgr) FetchPrivateEnv(id Workunit_Unique_Identifier, clientid string) (env map[string]string, err error) {
	//precheck if the client is registered
	client, ok, err := qm.GetClient(clientid, true)
	if err != nil {
		return
	}
	if !ok {
		return env, errors.New(e.ClientNotFound)
	}

	is_suspended, err := client.Get_Suspended(true)
	if err != nil {
		return
	}
	if is_suspended {
		err = errors.New(e.ClientSuspended)
		return
	}
	//jobid := id.JobId
	//taskid := id.TaskName

	//job, err := GetJob(jobid)

	task, ok, err := qm.TaskMap.Get(id.Task_Unique_Identifier, true)
	if err != nil {
		err = fmt.Errorf("(FetchPrivateEnv) qm.TaskMap.Get returned: %s", err.Error())
		return
	}

	if !ok {
		var task_str string
		task_str, err = task.String()
		if err != nil {
			err = fmt.Errorf("(FetchPrivateEnv) task.String returned: %s", err.Error())
			return
		}
		err = fmt.Errorf("(FetchPrivateEnv) task %s not found in qm.TaskMap", task_str)
		return
	}

	env = task.Cmd.Environ.Private
	return
	//env, err = dbGetPrivateEnv(jobid, taskid)
	//if err != nil {
	//	return
	//}

	//return
}
