package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/MG-RAST/AWE/lib/conf"
	e "github.com/MG-RAST/AWE/lib/errors"
	"github.com/MG-RAST/AWE/lib/logger"
	"github.com/MG-RAST/AWE/lib/logger/event"
	"github.com/MG-RAST/AWE/lib/user"
	"github.com/MG-RAST/golib/mgo/bson"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"time"
)

type ServerMgr struct {
	CQMgr
	taskMap map[string]*Task
	actJobs map[string]*JobPerf
	susJobs map[string]bool
	jsReq   chan bool   //channel for job submission request (JobController -> qmgr.Handler)
	jsAck   chan string //channel for return an assigned job number in response to jsReq  (qmgr.Handler -> JobController)
	taskIn  chan *Task  //channel for receiving Task (JobController -> qmgr.Handler)
	coSem   chan int    //semaphore for checkout (mutual exclusion between different clients)
	nextJid string      //next jid that will be assigned to newly submitted job
}

func NewServerMgr() *ServerMgr {
	return &ServerMgr{
		CQMgr: CQMgr{
			clientMap: map[string]*Client{},
			workQueue: NewWQueue(),
			reminder:  make(chan bool),
			coReq:     make(chan CoReq),
			coAck:     make(chan CoAck),
			feedback:  make(chan Notice),
			coSem:     make(chan int, 1), //non-blocking buffered channel
		},
		taskMap: map[string]*Task{},
		jsReq:   make(chan bool),
		jsAck:   make(chan string),
		taskIn:  make(chan *Task, 1024),
		actJobs: map[string]*JobPerf{},
		susJobs: map[string]bool{},
		nextJid: "",
	}
}

//to-do: consider separate some independent tasks into another goroutine to handle

func (qm *ServerMgr) Handle() {
	for {
		select {
		case <-qm.jsReq:
			jid := qm.getNextJid()
			qm.jsAck <- jid
			logger.Debug(2, fmt.Sprintf("qmgr:receive a job submission request, assigned jid=%s\n", jid))

		case task := <-qm.taskIn:
			logger.Debug(2, fmt.Sprintf("qmgr:task recived from chan taskIn, id=%s\n", task.Id))
			qm.addTask(task)

		case coReq := <-qm.coReq:
			logger.Debug(2, fmt.Sprintf("qmgr: workunit checkout request received, Req=%v\n", coReq))
			works, err := qm.popWorks(coReq)
			if err == nil {
				qm.UpdateJobTaskToInProgress(works)
			}
			ack := CoAck{workunits: works, err: err}
			qm.coAck <- ack

		case notice := <-qm.feedback:
			logger.Debug(2, fmt.Sprintf("qmgr: workunit feedback received, workid=%s, status=%s, clientid=%s\n", notice.WorkId, notice.Status, notice.ClientId))
			if err := qm.handleWorkStatusChange(notice); err != nil {
				logger.Error("handleWorkStatusChange(): " + err.Error())
			}

		case <-qm.reminder:
			logger.Debug(3, "time to update workunit queue....\n")
			qm.updateQueue()
			if conf.DEV_MODE {
				fmt.Println(qm.ShowStatus())
			}
		}
	}
}

func (qm *ServerMgr) Timer() {
	for {
		time.Sleep(10 * time.Second)
		qm.reminder <- true
	}
}

func (qm *ServerMgr) InitMaxJid() (err error) {
	jidfile := conf.DATA_PATH + "/maxjid"

	if _, err := os.Stat(jidfile); err != nil {

		f, err := os.Create(jidfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, fmt.Sprintf("error creating jidfile ", err.Error())) // logger does not work
			logger.Error(fmt.Sprintf("error creating jidfile ", err.Error()))
			return err
		}
		f.WriteString("10000")
		qm.nextJid = "10001"
		f.Close()
	} else {

		buf, err := ioutil.ReadFile(jidfile)
		if err != nil {
			if conf.DEBUG_LEVEL > 0 {
				fmt.Println("error ioutil.ReadFile(jidfile)")
			}
			return err
		}
		bufstr := strings.TrimSpace(string(buf))

		maxjid, err := strconv.Atoi(bufstr)
		if err != nil {
			if conf.DEBUG_LEVEL > 0 {
				fmt.Println(fmt.Sprintf("error strconv.Atoi(bufstr), bufstr=\"%s\"", bufstr))
			}
			fmt.Fprintf(os.Stderr, fmt.Sprintf("Could not convert \"%s\" into int", bufstr)) // logger does not work
			logger.Error(fmt.Sprintf("Could not convert \"%s\" into int", bufstr))
			return err
		}

		qm.nextJid = strconv.Itoa(maxjid + 1)

	}
	if conf.DEBUG_LEVEL > 0 {
		fmt.Println("in InitMaxJid C")
	}
	logger.Debug(2, fmt.Sprintf("qmgr:jid initialized, next jid=%s\n", qm.nextJid))
	return
}

//poll ready tasks and push into workQueue
func (qm *ServerMgr) updateQueue() (err error) {
	for _, task := range qm.taskMap {
		if qm.isTaskReady(task) {
			if err := qm.taskEnQueue(task); err != nil {
				jobid, _ := GetJobIdByTaskId(task.Id)
				qm.SuspendJob(jobid, fmt.Sprintf("failed enqueuing task %s, err=%s", task.Id, err.Error()), task.Id)
			}
		}
	}
	for id, work := range qm.workQueue.workMap {
		if work == nil || work.Info == nil {
			jid, err := GetJobIdByWorkId(id)
			if err != nil {
				qm.workQueue.Delete(id)
				logger.Error(fmt.Sprintf("error: in updateQueue() workunit %s is nil, cannot get job id", id))
				continue
			}

			if work == nil {
				qm.workQueue.Delete(id)
				qm.SuspendJob(jid, fmt.Sprintf("workunit %s is nil", id), id)
				logger.Error(fmt.Sprintf("error: workunit %s is nil, deleted from queue", id))
			} else if work.Info == nil {
				qm.workQueue.Delete(id)
				qm.SuspendJob(jid, fmt.Sprintf("workunit %s has Info=nil", id), id)
				logger.Error(fmt.Sprintf("error: workunit %s has Info=nil, deleted from queue", id))
			}
		}
	}
	return
}

//handle feedback from a client about the execution of a workunit
func (qm *ServerMgr) handleWorkStatusChange(notice Notice) (err error) {
	workid := notice.WorkId
	status := notice.Status
	clientid := notice.ClientId
	computetime := notice.ComputeTime
	parts := strings.Split(workid, "_")
	taskid := fmt.Sprintf("%s_%s", parts[0], parts[1])
	jobid := parts[0]
	rank, err := strconv.Atoi(parts[2])
	if err != nil {
		return errors.New(fmt.Sprintf("invalid workid %s", workid))
	}
	if _, ok := qm.clientMap[clientid]; ok {
		delete(qm.clientMap[clientid].Current_work, workid)
		if len(qm.clientMap[clientid].Current_work) == 0 {
			qm.clientMap[clientid].Status = CLIENT_STAT_ACTIVE_IDLE
		}
	}
	if task, ok := qm.taskMap[taskid]; ok {
		if _, ok := qm.workQueue.workMap[workid]; !ok {
			return
		}
		if qm.workQueue.workMap[workid].State != WORK_STAT_CHECKOUT { //could be suspended
			return
		}
		var MAX_FAILURE int
		if task.Info.NoRetry == true {
			MAX_FAILURE = 1
		} else {
			MAX_FAILURE = conf.MAX_WORK_FAILURE
		}
		if task.State == TASK_STAT_FAIL_SKIP {
			// A work unit for this task failed before this one arrived.
			// User set Skip=2 so the task was just skipped. Any subsiquent
			// workunits are just deleted...
			qm.workQueue.Delete(workid)
		} else {
			qm.updateTaskWorkStatus(taskid, rank, status)
			if status == WORK_STAT_DONE {
				//log event about work done (WD)
				logger.Event(event.WORK_DONE, "workid="+workid+";clientid="+clientid)

				//update client status
				if client, ok := qm.clientMap[clientid]; ok {
					client.Total_completed += 1
					client.Last_failed = 0 //reset last consecutive failures
				} else {
					//it happens when feedback is sent after server restarted and before client re-registered
				}
				task.RemainWork -= 1
				task.ComputeTime += computetime
				if task.RemainWork == 0 {
					task.State = TASK_STAT_COMPLETED
					task.CompletedDate = time.Now()
					for _, output := range task.Outputs {
						output.GetFileSize()
						output.DataUrl()
					}
					//log event about task done (TD)
					qm.FinalizeTaskPerf(taskid)
					logger.Event(event.TASK_DONE, "taskid="+taskid)
					//update the info of the job which the task is belong to, could result in deletion of the
					//task in the task map when the task is the final task of the job to be done.
					if err = qm.updateJobTask(task); err != nil { //task state QUEUED -> COMPELTED
						return
					}
					qm.updateQueue()
				}
				//done, remove from the workQueue
				qm.workQueue.Delete(workid)
			} else if status == WORK_STAT_FAIL { //workunit failed, requeue or put it to suspend list
				logger.Event(event.WORK_FAIL, "workid="+workid+";clientid="+clientid)
				if qm.workQueue.Has(workid) {
					qm.workQueue.workMap[workid].Failed += 1
					if task.Skip == 2 && task.Skippable() { // user wants to skip task

						task.RemainWork = 0 // not doing anything else...
						task.State = TASK_STAT_FAIL_SKIP
						for _, output := range task.Outputs {
							output.GetFileSize()
							output.DataUrl()
						}
						qm.FinalizeTaskPerf(taskid)
						// log event about task skipped
						logger.Event(event.TASK_SKIPPED, "taskid="+taskid)
						//update the info of the job which the task is belong to, could result in deletion of the
						//task in the task map when the task is the final task of the job to be done.
						if err = qm.updateJobTask(task); err != nil { //task state QUEUED -> FAIL_SKIP
							return
						}
						qm.updateQueue()
						// remove from the workQueue
						qm.workQueue.Delete(workid)
					} else if qm.workQueue.workMap[workid].Failed < MAX_FAILURE {
						qm.workQueue.StatusChange(workid, WORK_STAT_QUEUED)
						logger.Event(event.WORK_REQUEUE, "workid="+workid)
					} else { //failure time exceeds limit, suspend workunit, task, job
						qm.workQueue.StatusChange(workid, WORK_STAT_SUSPEND)
						logger.Event(event.WORK_SUSPEND, "workid="+workid)
						qm.updateTaskWorkStatus(taskid, rank, WORK_STAT_SUSPEND)
						qm.taskMap[taskid].State = TASK_STAT_SUSPEND

						reason := fmt.Sprintf("workunit %s failed %d time(s).", workid, MAX_FAILURE)
						if len(notice.Notes) > 0 {
							reason = reason + " msg from client:" + notice.Notes
						}
						if err := qm.SuspendJob(jobid, reason, workid); err != nil {
							logger.Error("error returned by SuspendJOb()" + err.Error())
						}
					}
				}
				if client, ok := qm.clientMap[clientid]; ok {
					client.Skip_work = append(client.Skip_work, workid)
					client.Total_failed += 1
					client.Last_failed += 1 //last consecutive failures
					if client.Last_failed == conf.MAX_CLIENT_FAILURE {
						qm.SuspendClient(client.Id)
					}
				}
			} else if status == WORK_STAT_DISCARDED { //workunit discarded,
				//do nothing
			}
		}
	} else { //task not existed, possible when job is deleted before the workunit done
		qm.workQueue.Delete(workid)
	}
	return
}

func (qm *ServerMgr) ShowStatus() string {
	total_task := len(qm.taskMap)
	queuing_work := len(qm.workQueue.wait)
	out_work := len(qm.workQueue.checkout)
	suspend_work := len(qm.workQueue.suspend)
	total_active_work := len(qm.workQueue.workMap)
	queuing_task := 0
	started_task := 0
	pending_task := 0
	completed_task := 0
	suspended_task := 0
	skipped_task := 0
	fail_skip_task := 0
	for _, task := range qm.taskMap {
		if task.State == TASK_STAT_COMPLETED {
			completed_task += 1
		} else if task.State == TASK_STAT_PENDING {
			pending_task += 1
		} else if task.State == TASK_STAT_QUEUED {
			queuing_task += 1
		} else if task.State == TASK_STAT_INPROGRESS {
			started_task += 1
		} else if task.State == TASK_STAT_SUSPEND {
			suspended_task += 1
		} else if task.State == TASK_STAT_SKIPPED {
			skipped_task += 1
		} else if task.State == TASK_STAT_FAIL_SKIP {
			fail_skip_task += 1
		}
	}
	total_task -= skipped_task // user doesn't see skipped tasks
	active_jobs := len(qm.actJobs)
	suspend_job := len(qm.susJobs)
	total_job := active_jobs + suspend_job
	busy_client := 0
	idle_client := 0
	suspend_client := 0
	for _, client := range qm.clientMap {
		if client.Status == CLIENT_STAT_SUSPEND {
			suspend_client += 1
		} else if client.IsBusy() {
			busy_client += 1
		} else {
			idle_client += 1
		}
	}

	statMsg := "++++++++AWE server queue status++++++++\n" +
		fmt.Sprintf("total jobs ............... %d\n", total_job) +
		fmt.Sprintf("    active:           (%d)\n", active_jobs) +
		fmt.Sprintf("    suspended:        (%d)\n", suspend_job) +
		fmt.Sprintf("total tasks .............. %d\n", total_task) +
		fmt.Sprintf("    queuing:          (%d)\n", queuing_task) +
		fmt.Sprintf("    in-progress:      (%d)\n", started_task) +
		fmt.Sprintf("    pending:          (%d)\n", pending_task) +
		fmt.Sprintf("    completed:        (%d)\n", completed_task) +
		fmt.Sprintf("    suspended:        (%d)\n", suspended_task) +
		fmt.Sprintf("    failed & skipped: (%d)\n", fail_skip_task) +
		fmt.Sprintf("total workunits .......... %d\n", total_active_work) +
		fmt.Sprintf("    queuing:          (%d)\n", queuing_work) +
		fmt.Sprintf("    checkout:         (%d)\n", out_work) +
		fmt.Sprintf("    suspended:        (%d)\n", suspend_work) +
		fmt.Sprintf("total clients ............ %d\n", len(qm.clientMap)) +
		fmt.Sprintf("    busy:             (%d)\n", busy_client) +
		fmt.Sprintf("    idle:             (%d)\n", idle_client) +
		fmt.Sprintf("    suspend:          (%d)\n", suspend_client) +
		fmt.Sprintf("---last update: %s\n\n", time.Now())
	return statMsg
}

//---end of mgr methods

//--workunit methds (servermgr implementation)
func (qm *ServerMgr) FetchDataToken(workid string, clientid string) (token string, err error) {
	//precheck if the client is registered
	if _, hasClient := qm.clientMap[clientid]; !hasClient {
		return "", errors.New(e.ClientNotFound)
	}
	if qm.clientMap[clientid].Status == CLIENT_STAT_SUSPEND {
		return "", errors.New(e.ClientSuspended)
	}
	jobid, err := GetJobIdByWorkId(workid)
	if err != nil {
		return "", err
	}
	job, err := LoadJob(jobid)
	if err != nil {
		return "", err
	}
	token = job.GetDataToken()
	if token == "" {
		return token, errors.New("no data token set for workunit " + workid)
	}
	return token, nil
}

func (qm *ServerMgr) FetchPrivateEnvs(workid string, clientid string) (envs map[string]string, err error) {
	//precheck if the client is registered
	if _, hasClient := qm.clientMap[clientid]; !hasClient {
		return nil, errors.New(e.ClientNotFound)
	}
	if qm.clientMap[clientid].Status == CLIENT_STAT_SUSPEND {
		return nil, errors.New(e.ClientSuspended)
	}
	jobid, err := GetJobIdByWorkId(workid)
	if err != nil {
		return nil, err
	}

	job, err := LoadJob(jobid)
	if err != nil {
		return nil, err
	}

	taskid, _ := GetTaskIdByWorkId(workid)

	idx := -1
	for i, t := range job.Tasks {
		if t.Id == taskid {
			idx = i
			break
		}
	}
	envs = job.Tasks[idx].Cmd.Environ.Private
	if envs == nil {
		return nil, errors.New("no private envs for workunit " + workid)
	}
	return envs, nil
}

func (qm *ServerMgr) SaveStdLog(workid string, logname string, tmppath string) (err error) {
	savedpath, err := getStdLogPathByWorkId(workid, logname)
	if err != nil {
		return err
	}
	os.Rename(tmppath, savedpath)
	return
}

func (qm *ServerMgr) GetReportMsg(workid string, logname string) (report string, err error) {
	logpath, err := getStdLogPathByWorkId(workid, logname)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(logpath); err != nil {
		return "", errors.New("log type '" + logname + "' not found")
	} else {
		fmt.Printf("fi=%v\n", fi)
	}

	content, err := ioutil.ReadFile(logpath)
	if err != nil {
		return "", err
	}
	return string(content), err
}

func getStdLogPathByWorkId(workid string, logname string) (string, error) {
	jobid, err := GetJobIdByWorkId(workid)
	if err != nil {
		return "", err
	}
	logdir := getPathByJobId(jobid)
	savedpath := fmt.Sprintf("%s/%s.%s", logdir, workid, logname)
	return savedpath, nil
}

//---task methods----

func (qm *ServerMgr) EnqueueTasksByJobId(jobid string, tasks []*Task) (err error) {
	for _, task := range tasks {
		if task.Info == nil {
			fmt.Printf("task.Info is nil for job: %v\n", jobid)
		}
		qm.taskIn <- task
	}
	qm.CreateJobPerf(jobid)
	return
}

//---end of task methods

func (qm *ServerMgr) addTask(task *Task) (err error) {
	id := task.Id
	if task.State == TASK_STAT_COMPLETED { //for job recovery from db
		qm.taskMap[id] = task
		return
	}
	if task.Skip == 1 && task.Skippable() {
		qm.skipTask(task)
		return
	}

	if task.State == TASK_STAT_PASSED { //for pseudo-task
		qm.taskMap[id] = task
		return
	}

	task.State = TASK_STAT_PENDING
	qm.taskMap[id] = task
	if qm.isTaskReady(task) {
		if err := qm.taskEnQueue(task); err != nil {
			jobid, _ := GetJobIdByTaskId(task.Id)
			qm.SuspendJob(jobid, fmt.Sprintf("failed in enqueuing task %s, err=%s", task.Id, err.Error()), task.Id)
			return err
		}
	}
	if err = qm.updateJobTask(task); err != nil { //task state INIT->PENDING
		return
	}
	return
}

func (qm *ServerMgr) skipTask(task *Task) (err error) {
	task.State = TASK_STAT_SKIPPED
	task.RemainWork = 0
	//update job and queue info. Skipped task behaves as finished tasks
	if err = qm.updateJobTask(task); err != nil { //TASK state  -> SKIPPED
		return
	}
	qm.taskMap[task.Id] = task
	logger.Event(event.TASK_SKIPPED, "taskid="+task.Id)
	return
}

//delete task from taskMap
func (qm *ServerMgr) deleteTasks(tasks []*Task) (err error) {
	return
}

//check whether a pending task is ready to enqueue (dependent tasks are all done)
func (qm *ServerMgr) isTaskReady(task *Task) (ready bool) {
	ready = false

	//skip if the belonging job is suspended
	jobid, _ := GetJobIdByTaskId(task.Id)
	if _, ok := qm.susJobs[jobid]; ok {
		return false
	}

	if task.State == TASK_STAT_PENDING {
		ready = true
		for _, predecessor := range task.DependsOn {
			if _, haskey := qm.taskMap[predecessor]; haskey {
				if qm.taskMap[predecessor].State != TASK_STAT_COMPLETED &&
					qm.taskMap[predecessor].State != TASK_STAT_PASSED &&
					qm.taskMap[predecessor].State != TASK_STAT_SKIPPED &&
					qm.taskMap[predecessor].State != TASK_STAT_FAIL_SKIP {
					ready = false
				} else {
					logger.Error("warning: predecessor " + predecessor + " is unknown")
				}
			}
		}
	}
	if task.Skip == 1 && task.Skippable() {
		qm.skipTask(task)
		ready = false
	}
	return
}

func (qm *ServerMgr) taskEnQueue(task *Task) (err error) {

	logger.Debug(2, "trying to enqueue task "+task.Id)

	if err := qm.locateInputs(task); err != nil {
		logger.Error("qmgr.taskEnQueue locateInputs:" + err.Error())
		return err
	}

	//create shock index on input nodes (if set in workflow document)
	if err := task.CreateIndex(); err != nil {
		logger.Error("qmgr.taskEnQueue CreateIndex:" + err.Error())
		return err
	}

	//init partition
	if err := task.InitPartIndex(); err != nil {
		logger.Error("qmgr.taskEnQueue InitPartitionIndex:" + err.Error())
		return err
	}

	if err := qm.createOutputNode(task); err != nil {
		logger.Error("qmgr.taskEnQueue createOutputNode:" + err.Error())
		return err
	}
	if err := qm.parseTask(task); err != nil {
		logger.Error("qmgr.taskEnQueue parseTask:" + err.Error())
		return err
	}
	task.State = TASK_STAT_QUEUED
	task.CreatedDate = time.Now()
	task.StartedDate = time.Now() //to-do: will be changed to the time when the first workunit is checked out
	qm.updateJobTask(task)        //task status PENDING->QUEUED

	//log event about task enqueue (TQ)
	logger.Event(event.TASK_ENQUEUE, fmt.Sprintf("taskid=%s;totalwork=%d", task.Id, task.TotalWork))
	qm.CreateTaskPerf(task.Id)

	if IsFirstTask(task.Id) {
		jobid, _ := GetJobIdByTaskId(task.Id)
		UpdateJobState(jobid, JOB_STAT_QUEUED, []string{JOB_STAT_INIT, JOB_STAT_SUSPEND})
	}
	return
}

func (qm *ServerMgr) locateInputs(task *Task) (err error) {
	logger.Debug(2, "trying to locate Inputs of task "+task.Id)
	jobid, _ := GetJobIdByTaskId(task.Id)
	for name, io := range task.Inputs {
		if io.Url == "" {
			preId := fmt.Sprintf("%s_%s", jobid, io.Origin)
			if preTask, ok := qm.taskMap[preId]; ok {
				if preTask.State == TASK_STAT_SKIPPED ||
					preTask.State == TASK_STAT_FAIL_SKIP {
					// For now we know that skipped tasks have
					// just one input and one output. So we know
					// that we just need to change one file (this
					// may change in the future)
					//locateSkippedInput(qm, preTask, io)
				} else {
					outputs := preTask.Outputs
					if outio, ok := outputs[name]; ok {
						io.Node = outio.Node
					}
				}
			}
		}
		io.DataUrl()
		if io.Node == "-" {
			return errors.New(fmt.Sprintf("error in locate input for task %s, %s", task.Id, name))
		}
		//need time out!
		if io.Node != "" && io.GetFileSize() < 0 {
			return errors.New(fmt.Sprintf("task %s: input file %s not available", task.Id, name))
		}
		logger.Debug(2, fmt.Sprintf("inputs located %s, %s\n", name, io.Node))
	}
	return
}

func (qm *ServerMgr) parseTask(task *Task) (err error) {
	workunits, err := task.ParseWorkunit()
	if err != nil {
		return err
	}
	for _, wu := range workunits {
		if err := qm.workQueue.Add(wu); err != nil {
			return err
		}
		qm.CreateWorkPerf(wu.Id)
	}
	return
}

func (qm *ServerMgr) createOutputNode(task *Task) (err error) {

	outputs := task.Outputs
	for name, io := range outputs {
		if io.Type == "update" {
			// this an update output, it will update an existing shock node and not create a new one
			io.DataUrl()
			if (io.Node == "") || (io.Node == "-") {
				if io.Origin == "" {
					return errors.New(fmt.Sprintf("update output %s in task %s is missing required origin", name, task.Id))
				}
				nodeid, err := qm.locateUpdate(task.Id, name, io.Origin)
				if err != nil {
					return err
				}
				io.Node = nodeid
			}
			logger.Debug(2, fmt.Sprintf("outout %s in task %s is an update of node %s\n", name, task.Id, io.Node))
		} else {
			// POST empty shock node for this output
			logger.Debug(2, fmt.Sprintf("posting output Shock node for file %s in task %s\n", name, task.Id))
			nodeid, err := PostNodeWithToken(io, task.TotalWork, task.Info.DataToken)
			if err != nil {
				return err
			}
			io.Node = nodeid
			logger.Debug(2, fmt.Sprintf("task %s: output Shock node created, node=%s\n", task.Id, nodeid))
		}
	}
	return
}

func (qm *ServerMgr) locateUpdate(taskid string, name string, origin string) (nodeid string, err error) {
	jobid, _ := GetJobIdByTaskId(taskid)
	preId := fmt.Sprintf("%s_%s", jobid, origin)
	logger.Debug(2, fmt.Sprintf("task %s: trying to locate Node of update %s from task %s", taskid, name, preId))
	// scan outputs in origin task
	if preTask, ok := qm.taskMap[preId]; ok {
		outputs := preTask.Outputs
		if outio, ok := outputs[name]; ok {
			return outio.Node, nil
		}
	}
	return "", errors.New(fmt.Sprintf("failed to locate Node for task %s / update %s from task %s", taskid, name, preId))
}

func (qm *ServerMgr) updateTaskWorkStatus(taskid string, rank int, newstatus string) {
	if _, ok := qm.taskMap[taskid]; !ok {
		return
	}
	if rank == 0 {
		qm.taskMap[taskid].WorkStatus[rank] = newstatus
	} else {
		qm.taskMap[taskid].WorkStatus[rank-1] = newstatus
	}
	return
}

func (qm *ServerMgr) ShowTasks() {
	fmt.Printf("current active tasks  (%d):\n", len(qm.taskMap))
	for key, task := range qm.taskMap {
		fmt.Printf("workunit id: %s, status:%s\n", key, task.State)
	}
}

//---end of task methods---

//---job methods---
func (qm *ServerMgr) JobRegister() (jid string, err error) {
	qm.jsReq <- true
	jid = <-qm.jsAck

	if jid == "" {
		return "", errors.New("failed to assign a job number for the newly submitted job")
	}
	return jid, nil
}

func (qm *ServerMgr) getNextJid() (jid string) {
	jid = qm.nextJid
	jidfile := conf.DATA_PATH + "/maxjid"
	ioutil.WriteFile(jidfile, []byte(jid), 0644)
	qm.nextJid = jidIncr(qm.nextJid)
	return jid
}

//update job info when a task in that job changed to a new state
func (qm *ServerMgr) updateJobTask(task *Task) (err error) {
	parts := strings.Split(task.Id, "_")
	jobid := parts[0]
	job, err := LoadJob(jobid)
	if err != nil {
		return
	}
	remainTasks, err := job.UpdateTask(task)
	if err != nil {
		return err
	}

	logger.Debug(2, fmt.Sprintf("remaining tasks for task %s: %d", task.Id, remainTasks))

	if remainTasks == 0 { //job done
		qm.FinalizeJobPerf(jobid)
		qm.LogJobPerf(jobid)
		qm.DeleteJobPerf(jobid)
		//delete tasks in task map
		//delete from shock output flagged for deletion
		for _, task := range job.TaskList() {
			task.DeleteOutput()
			delete(qm.taskMap, task.Id)
		}
		//log event about job done (JD)
		logger.Event(event.JOB_DONE, "jobid="+job.Id+";jid="+job.Jid+";project="+job.Info.Project+";name="+job.Info.Name)
	}
	return
}

//update job/task states from "queued" to "in-progress" once the first workunit is checked out
func (qm *ServerMgr) UpdateJobTaskToInProgress(works []*Workunit) {
	for _, work := range works {
		job_was_inprogress := false
		task_was_inprogress := false
		taskid, _ := GetTaskIdByWorkId(work.Id)
		jobid, _ := GetJobIdByWorkId(work.Id)
		//Load job by id
		job, err := LoadJob(jobid)
		if err != nil {
			continue
		}
		//update job status
		if job.State == JOB_STAT_INPROGRESS {
			job_was_inprogress = true
		} else {
			job.State = JOB_STAT_INPROGRESS
			job.Info.StartedTme = time.Now()
			qm.UpdateJobPerfStartTime(jobid)
		}
		//update task status
		idx := -1
		for i, t := range job.Tasks {
			if t.Id == taskid {
				idx = i
				break
			}
		}
		if idx == -1 {
			continue
		}
		if job.Tasks[idx].State == TASK_STAT_INPROGRESS {
			task_was_inprogress = true
		} else {
			job.Tasks[idx].State = TASK_STAT_INPROGRESS
			if _, ok := qm.taskMap[taskid]; ok {
				qm.taskMap[taskid].State = TASK_STAT_INPROGRESS
			}
			job.Tasks[idx].StartedDate = time.Now()
			qm.UpdateTaskPerfStartTime(taskid)
		}

		if !job_was_inprogress || !task_was_inprogress {
			job.Save()
		}
	}
}

func (qm *ServerMgr) GetActiveJobs() map[string]*JobPerf {
	return qm.actJobs
}

func (qm *ServerMgr) IsJobRegistered(id string) bool {
	if _, ok := qm.actJobs[id]; ok {
		return true
	}
	if _, ok := qm.susJobs[id]; ok {
		return true
	}
	return false
}

func (qm *ServerMgr) GetSuspendJobs() map[string]bool {
	return qm.susJobs
}

func (qm *ServerMgr) SuspendJob(jobid string, reason string, id string) (err error) {
	job, err := LoadJob(jobid)
	if err != nil {
		return
	}
	if id != "" {
		job.LastFailed = id
	}
	if err := job.UpdateState(JOB_STAT_SUSPEND, reason); err != nil {
		return err
	}
	//qm.DeleteJobPerf(jobid)
	qm.susJobs[jobid] = true

	//suspend queueing workunits
	for workid, _ := range qm.workQueue.workMap {
		if jobid == strings.Split(workid, "_")[0] {
			qm.workQueue.StatusChange(workid, WORK_STAT_SUSPEND)
		}
	}

	//suspend parsed tasks
	for _, task := range job.Tasks {
		if task.State == TASK_STAT_QUEUED || task.State == TASK_STAT_INIT || task.State == TASK_STAT_INPROGRESS {
			if _, ok := qm.taskMap[task.Id]; ok {
				qm.taskMap[task.Id].State = TASK_STAT_SUSPEND
				task.State = TASK_STAT_SUSPEND
				job.UpdateTask(task)
			}
		}
	}
	qm.LogJobPerf(jobid)
	qm.DeleteJobPerf(jobid)
	logger.Event(event.JOB_SUSPEND, "jobid="+jobid+";reason="+reason)
	return
}

func (qm *ServerMgr) DeleteJob(jobid string) (err error) {
	job, err := LoadJob(jobid)
	if err != nil {
		return
	}
	if err := job.UpdateState(JOB_STAT_DELETED, "deleted"); err != nil {
		return err
	}
	//delete queueing workunits
	for workid, _ := range qm.workQueue.workMap {
		if jobid == strings.Split(workid, "_")[0] {
			qm.workQueue.Delete(workid)
		}
	}
	//delete parsed tasks
	for i := 0; i < len(job.TaskList()); i++ {
		task_id := fmt.Sprintf("%s_%d", jobid, i)
		delete(qm.taskMap, task_id)
	}
	qm.DeleteJobPerf(jobid)
	delete(qm.susJobs, jobid)

	logger.Event(event.JOB_DELETED, "jobid="+jobid)
	return
}

func (qm *ServerMgr) DeleteJobByUser(jobid string, u *user.User) (err error) {
	job, err := LoadJob(jobid)
	if err != nil {
		return
	}
	// User must have delete permissions on job or be job owner or be an admin
	rights := job.Acl.Check(u.Uuid)
	if job.Acl.Owner != u.Uuid && rights["delete"] == false && u.Admin == false {
		return errors.New(e.UnAuth)
	}
	if err := job.UpdateState(JOB_STAT_DELETED, "deleted"); err != nil {
		return err
	}
	//delete queueing workunits
	for workid, _ := range qm.workQueue.workMap {
		if jobid == strings.Split(workid, "_")[0] {
			qm.workQueue.Delete(workid)
		}
	}
	//delete parsed tasks
	for i := 0; i < len(job.TaskList()); i++ {
		task_id := fmt.Sprintf("%s_%d", jobid, i)
		delete(qm.taskMap, task_id)
	}
	qm.DeleteJobPerf(jobid)
	delete(qm.susJobs, jobid)

	logger.Event(event.JOB_DELETED, "jobid="+jobid)
	return
}

func (qm *ServerMgr) DeleteSuspendedJobs() (num int) {
	suspendjobs := qm.GetSuspendJobs()
	for id, _ := range suspendjobs {
		if err := qm.DeleteJob(id); err == nil {
			num += 1
		}
	}
	return
}

func (qm *ServerMgr) DeleteSuspendedJobsByUser(u *user.User) (num int) {
	suspendjobs := qm.GetSuspendJobs()
	for id, _ := range suspendjobs {
		if err := qm.DeleteJobByUser(id, u); err == nil {
			num += 1
		}
	}
	return
}

func (qm *ServerMgr) ResumeSuspendedJobs() (num int) {
	suspendjobs := qm.GetSuspendJobs()
	for id, _ := range suspendjobs {
		if err := qm.ResumeSuspendedJob(id); err == nil {
			num += 1
		}
	}
	return
}

func (qm *ServerMgr) ResumeSuspendedJobsByUser(u *user.User) (num int) {
	suspendjobs := qm.GetSuspendJobs()
	for id, _ := range suspendjobs {
		if err := qm.ResumeSuspendedJobByUser(id, u); err == nil {
			num += 1
		}
	}
	return
}

//delete jobs in db with "queued" or "in-progress" state but not in the queue (zombie jobs)
func (qm *ServerMgr) DeleteZombieJobs() (num int) {
	dbjobs := new(Jobs)
	q := bson.M{}
	q["state"] = bson.M{"in": JOB_STATS_ACTIVE}
	if err := dbjobs.GetAll(q, "info.submittime", "asc"); err != nil {
		logger.Error("DeleteZombieJobs()->GetAllLimitOffset():" + err.Error())
		return
	}
	for _, dbjob := range *dbjobs {
		if _, ok := qm.actJobs[dbjob.Id]; !ok {
			if err := qm.DeleteJob(dbjob.Id); err == nil {
				num += 1
			}
		}
	}
	return
}

//delete jobs in db with "queued" or "in-progress" state but not in the queue (zombie jobs) that user has access to
func (qm *ServerMgr) DeleteZombieJobsByUser(u *user.User) (num int) {
	dbjobs := new(Jobs)
	q := bson.M{}
	q["state"] = bson.M{"in": JOB_STATS_ACTIVE}
	if err := dbjobs.GetAll(q, "info.submittime", "asc"); err != nil {
		logger.Error("DeleteZombieJobs()->GetAllLimitOffset():" + err.Error())
		return
	}
	for _, dbjob := range *dbjobs {
		if _, ok := qm.actJobs[dbjob.Id]; !ok {
			if err := qm.DeleteJobByUser(dbjob.Id, u); err == nil {
				num += 1
			}
		}
	}
	return
}

//resubmit a suspended job
func (qm *ServerMgr) ResumeSuspendedJob(id string) (err error) {
	//Load job by id
	dbjob, err := LoadJob(id)
	if err != nil {
		return errors.New("failed to load job " + err.Error())
	}
	if dbjob.State != JOB_STAT_SUSPEND {
		return errors.New("job " + id + " is not in 'suspend' status")
	}
	qm.EnqueueTasksByJobId(dbjob.Id, dbjob.TaskList())

	if dbjob.RemainTasks < len(dbjob.Tasks) {
		dbjob.State = JOB_STAT_INPROGRESS
	} else {
		dbjob.State = JOB_STAT_QUEUED
	}
	dbjob.Resumed += 1
	dbjob.Save()

	delete(qm.susJobs, id)
	return
}

//resubmit a suspended job if the user is authorized
func (qm *ServerMgr) ResumeSuspendedJobByUser(id string, u *user.User) (err error) {
	//Load job by id
	dbjob, err := LoadJob(id)
	if err != nil {
		return errors.New("failed to load job " + err.Error())
	}

	// User must have write permissions on job or be job owner or be an admin
	rights := dbjob.Acl.Check(u.Uuid)
	if dbjob.Acl.Owner != u.Uuid && rights["write"] == false && u.Admin == false {
		return errors.New(e.UnAuth)
	}

	if dbjob.State != JOB_STAT_SUSPEND {
		return errors.New("job " + id + " is not in 'suspend' status")
	}
	qm.EnqueueTasksByJobId(dbjob.Id, dbjob.TaskList())

	if dbjob.RemainTasks < len(dbjob.Tasks) {
		dbjob.State = JOB_STAT_INPROGRESS
	} else {
		dbjob.State = JOB_STAT_QUEUED
	}
	dbjob.Resumed += 1
	dbjob.Save()

	delete(qm.susJobs, id)
	return
}

//re-submit a job in db but not in the queue (caused by server restarting)
func (qm *ServerMgr) ResubmitJob(id string) (err error) {
	//Load job by id
	if _, ok := qm.actJobs[id]; ok {
		return errors.New("job " + id + " is already active")
	}
	dbjob, err := LoadJob(id)

	if err != nil {
		return errors.New("failed to load job " + err.Error())
	}
	if dbjob.State == JOB_STAT_COMPLETED ||
		dbjob.State == JOB_STAT_DELETED {
		return errors.New("job is in " + dbjob.State + "  state thus cannot be recovered")
	}
	for _, task := range dbjob.Tasks {
		task.Info = dbjob.Info
	}
	qm.EnqueueTasksByJobId(dbjob.Id, dbjob.TaskList())
	return
}

//recover jobs not completed before awe-server restarts
func (qm *ServerMgr) RecoverJobs() (err error) {
	//Get jobs to be recovered from db whose states are "submitted"
	dbjobs := new(Jobs)
	q := bson.M{}
	q["state"] = bson.M{"$in": JOB_STATS_TO_RECOVER}
	if err := dbjobs.GetAll(q, "info.submittime", "asc"); err != nil {
		logger.Error("RecoverJobs()->GetAllLimitOffset():" + err.Error())
		return err
	}
	//Locate the job script and parse tasks for each job
	jobct := 0
	for _, dbjob := range *dbjobs {
		if dbjob.State == "JOB_STAT_TO_SUSPEND" {
			qm.susJobs[dbjob.Id] = true //suspended jobs recovered as suspended
		} else {
			qm.EnqueueTasksByJobId(dbjob.Id, dbjob.TaskList())
		}
		jobct += 1
	}
	fmt.Printf("%d unfinished jobs recovered\n", jobct)
	return
}

//recompute jobs from specified task stage
func (qm *ServerMgr) RecomputeJob(jobid string, stage string) (err error) {
	if _, ok := qm.actJobs[jobid]; ok {
		return errors.New("job " + jobid + " is already active")
	}
	//Load job by id
	dbjob, err := LoadJob(jobid)
	if err != nil {
		return errors.New("failed to load job " + err.Error())
	}
	if dbjob.State != JOB_STAT_COMPLETED && dbjob.State != JOB_STAT_SUSPEND {
		return errors.New("job " + jobid + " is not in 'completed' or 'suspend' status")
	}

	was_suspend := false
	if dbjob.State == JOB_STAT_SUSPEND {
		was_suspend = true
	}

	from_task_id := fmt.Sprintf("%s_%s", jobid, stage)
	remaintasks := 0
	found := false
	for _, task := range dbjob.Tasks {
		if task.Id == from_task_id {
			resetTask(task)
			remaintasks += 1
			found = true
		}
	}
	if !found {
		return errors.New("task not found:" + from_task_id)
	}
	for _, task := range dbjob.Tasks {
		if isAncestor(dbjob, task.Id, from_task_id) {
			resetTask(task)
			remaintasks += 1
		}
	}
	qm.EnqueueTasksByJobId(dbjob.Id, dbjob.TaskList())
	dbjob.RemainTasks = remaintasks
	if dbjob.RemainTasks < len(dbjob.Tasks) {
		dbjob.UpdateState(JOB_STAT_INPROGRESS, "recomputed from task "+from_task_id)
	} else {
		dbjob.UpdateState(JOB_STAT_QUEUED, "recomputed from task "+from_task_id)
	}

	if was_suspend {
		delete(qm.susJobs, dbjob.Id)
	}

	return
}

func resetTask(task *Task) {
	task.State = TASK_STAT_PENDING
	task.RemainWork = task.TotalWork
	for _, input := range task.Inputs {
		if input.Origin != "" {
			input.Node = "-"
			input.Url = ""
			input.Size = 0
		}
	}
	for _, output := range task.Outputs {
		output.Node = "-"
		output.Url = ""
		output.Size = 0
	}
}

func isAncestor(job *Job, taskId string, testId string) bool {
	if taskId == testId {
		return false
	}
	idx := -1
	for i, t := range job.Tasks {
		if t.Id == taskId {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false
	}

	task := job.Tasks[idx]
	if len(task.DependsOn) == 0 {
		return false
	}
	if contains(task.DependsOn, testId) {
		return true
	} else {
		for _, t := range task.DependsOn {
			return isAncestor(job, t, testId)
		}
	}
	return false
}

//update job group
func (qm *ServerMgr) UpdateGroup(jobid string, newgroup string) (err error) {
	//update info in db
	dbjob, err := LoadJob(jobid)
	if err != nil {
		return errors.New("failed to load job " + err.Error())
	}
	dbjob.Info.ClientGroups = newgroup
	for _, task := range dbjob.Tasks {
		task.Info.ClientGroups = newgroup
	}
	dbjob.Save()

	//update in-memory data structures
	for workid, _ := range qm.workQueue.workMap {
		if jobid == strings.Split(workid, "_")[0] {
			qm.workQueue.workMap[workid].Info.ClientGroups = newgroup
		}
	}
	for _, task := range dbjob.Tasks {
		if _, ok := qm.taskMap[task.Id]; ok {
			qm.taskMap[task.Id].Info.ClientGroups = newgroup
		}
	}
	return
}

func (qm *ServerMgr) UpdatePriority(jobid string, priority int) (err error) {
	//update info in db
	dbjob, err := LoadJob(jobid)
	if err != nil {
		return errors.New("failed to load job " + err.Error())
	}
	dbjob.Info.Priority = priority
	for _, task := range dbjob.Tasks {
		task.Info.Priority = priority
	}
	dbjob.Save()

	//update in-memory data structures
	for workid, _ := range qm.workQueue.workMap {
		if jobid == strings.Split(workid, "_")[0] {
			qm.workQueue.workMap[workid].Info.Priority = priority
		}
	}
	for _, task := range dbjob.Tasks {
		if _, ok := qm.taskMap[task.Id]; ok {
			qm.taskMap[task.Id].Info.Priority = priority
		}
	}
	return
}

//---end of job methods

//---perf related methods
func (qm *ServerMgr) CreateJobPerf(jobid string) {
	if _, ok := qm.actJobs[jobid]; !ok {
		qm.actJobs[jobid] = NewJobPerf(jobid)
	}
}

func (qm *ServerMgr) UpdateJobPerfStartTime(jobid string) {
	if perf, ok := qm.actJobs[jobid]; ok {
		now := time.Now().Unix()
		perf.Start = now
	}
	return
}

func (qm *ServerMgr) FinalizeJobPerf(jobid string) {
	if perf, ok := qm.actJobs[jobid]; ok {
		now := time.Now().Unix()
		perf.End = now
		perf.Resp = now - perf.Queued
	}
	return
}

func (qm *ServerMgr) CreateTaskPerf(taskid string) {
	jobid := getParentJobId(taskid)
	if perf, ok := qm.actJobs[jobid]; ok {
		perf.Ptasks[taskid] = NewTaskPerf(taskid)
	}
}

func (qm *ServerMgr) UpdateTaskPerfStartTime(taskid string) {
	jobid := getParentJobId(taskid)
	if jobperf, ok := qm.actJobs[jobid]; ok {
		if taskperf, ok := jobperf.Ptasks[taskid]; ok {
			now := time.Now().Unix()
			taskperf.Start = now
		}
	}
}

func (qm *ServerMgr) FinalizeTaskPerf(taskid string) {
	jobid := getParentJobId(taskid)
	if jobperf, ok := qm.actJobs[jobid]; ok {
		if taskperf, ok := jobperf.Ptasks[taskid]; ok {
			now := time.Now().Unix()
			taskperf.End = now
			taskperf.Resp = now - taskperf.Queued
			//qm.actJobs[jobid].Ptasks[taskid] = taskperf

			if task, ok := qm.taskMap[taskid]; ok {
				for _, io := range task.Inputs {
					taskperf.InFileSizes = append(taskperf.InFileSizes, io.Size)
				}
				for _, io := range task.Outputs {
					taskperf.OutFileSizes = append(taskperf.OutFileSizes, io.Size)
				}
			}
			return
		}
	}
}

func (qm *ServerMgr) CreateWorkPerf(workid string) {
	if !conf.PERF_LOG_WORKUNIT {
		return
	}
	jobid := getParentJobId(workid)
	if _, ok := qm.actJobs[jobid]; !ok {
		return
	}
	qm.actJobs[jobid].Pworks[workid] = NewWorkPerf(workid)
}

func (qm *ServerMgr) FinalizeWorkPerf(workid string, reportfile string) (err error) {
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
	jobid := getParentJobId(workid)
	if _, ok := qm.actJobs[jobid]; !ok {
		return errors.New("job perf not found:" + jobid)
	}
	if _, ok := qm.actJobs[jobid].Pworks[workid]; !ok {
		return errors.New("work perf not found:" + workid)
	}
	workperf.Queued = qm.actJobs[jobid].Pworks[workid].Queued
	workperf.Done = time.Now().Unix()
	workperf.Resp = workperf.Done - workperf.Queued
	qm.actJobs[jobid].Pworks[workid] = workperf
	os.Remove(reportfile)
	return
}

func (qm *ServerMgr) LogJobPerf(jobid string) {
	if perf, ok := qm.actJobs[jobid]; ok {
		perfstr, _ := json.Marshal(perf)
		logger.Perf(string(perfstr)) //write into perf log
		dbUpsert(perf)               //write into mongodb
	}
}

func (qm *ServerMgr) DeleteJobPerf(jobid string) {
	delete(qm.actJobs, jobid)
}

//---end of perf related methods

func (qm *ServerMgr) FetchPrivateEnv(workid string, clientid string) (env map[string]string, err error) {
	//precheck if the client is registered
	if _, hasClient := qm.clientMap[clientid]; !hasClient {
		return env, errors.New(e.ClientNotFound)
	}
	if qm.clientMap[clientid].Status == CLIENT_STAT_SUSPEND {
		return env, errors.New(e.ClientSuspended)
	}
	jobid, err := GetJobIdByWorkId(workid)
	if err != nil {
		return env, err
	}
	job, err := LoadJob(jobid)
	if err != nil {
		return env, err
	}
	taskid, err := GetTaskIdByWorkId(workid)
	env = job.GetPrivateEnv(taskid)
	return env, nil
}
