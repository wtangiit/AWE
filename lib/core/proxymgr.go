package core

import (
	"fmt"
	"github.com/MG-RAST/AWE/lib/conf"
	"github.com/MG-RAST/AWE/lib/logger"
	"time"
)

type ProxyMgr struct {
	CQMgr
}

func NewProxyMgr() *ProxyMgr {
	return &ProxyMgr{
		CQMgr: CQMgr{
			clientMap: map[string]*Client{},
			workQueue: NewWQueue(),
			reminder:  make(chan bool),
			coReq:     make(chan CoReq),
			coAck:     make(chan CoAck),
			feedback:  make(chan Notice),
			coSem:     make(chan int, 1), //non-blocking buffered channel
		},
	}
}

//to-do: consider separate some independent tasks into another goroutine to handle

func (qm *ProxyMgr) Handle() {
	for {
		select {
		case coReq := <-qm.coReq:
			logger.Debug(2, fmt.Sprintf("proxymgr: workunit checkout request received, Req=%v\n", coReq))
			works, err := qm.popWorks(coReq)
			ack := CoAck{workunits: works, err: err}
			qm.coAck <- ack

		case notice := <-qm.feedback:
			logger.Debug(2, fmt.Sprintf("proxymgr: workunit feedback received, workid=%s, status=%s, clientid=%s\n", notice.WorkId, notice.Status, notice.ClientId))
			if err := qm.handleWorkStatusChange(notice); err != nil {
				logger.Error("handleWorkStatusChange(): " + err.Error())
			}
		case <-qm.reminder:
			logger.Debug(3, "time to update workunit queue....\n")
			if conf.DEV_MODE {
				fmt.Println(qm.ShowStatus())
			}
		}
	}
}

func (qm *ProxyMgr) Timer() {
	for {
		time.Sleep(10 * time.Second)
		qm.reminder <- true
	}
}

func (qm *ProxyMgr) InitMaxJid() (err error) {
	return
}

//handle feedback from a client about the execution of a workunit
func (qm *ProxyMgr) handleWorkStatusChange(notice Notice) (err error) {
	return
}

func (qm *ProxyMgr) ShowStatus() string {
	return ""
}

//---end of mgr methods

//---task methods----

func (qm *ProxyMgr) EnqueueTasksByJobId(jobid string, tasks []*Task) (err error) {
	return
}

//---end of task methods

//---job methods---
func (qm *ProxyMgr) JobRegister() (jid string, err error) {
	return
}

func (qm *ProxyMgr) GetActiveJobs() map[string]*JobPerf {
	return nil
}

func (qm *ProxyMgr) GetSuspendJobs() map[string]bool {
	return nil
}

func (qm *ProxyMgr) SuspendJob(jobid string, reason string) (err error) {
	return
}

func (qm *ProxyMgr) DeleteJob(jobid string) (err error) {
	return
}

func (qm *ProxyMgr) DeleteSuspendedJobs() (num int) {
	return
}

//resubmit a suspended job
func (qm *ProxyMgr) ResumeSuspendedJob(id string) (err error) {
	//Load job by id
	return
}

//re-submit a job in db but not in the queue (caused by server restarting)
func (qm *ProxyMgr) ResubmitJob(id string) (err error) {
	return
}

//recover jobs not completed before awe-server restarts
func (qm *ProxyMgr) RecoverJobs() (err error) {
	return
}

func (qm *ProxyMgr) FinalizeWorkPerf(string, string) (err error) {
	return
}

//---end of job methods
