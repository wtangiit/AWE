package controller

import (
	"github.com/MG-RAST/AWE/lib/conf"
	"github.com/MG-RAST/AWE/lib/core"
	e "github.com/MG-RAST/AWE/lib/errors"
	"github.com/MG-RAST/AWE/lib/logger"
	"github.com/MG-RAST/AWE/lib/logger/event"
	"github.com/MG-RAST/AWE/lib/request"
	"github.com/MG-RAST/AWE/lib/user"
	"github.com/MG-RAST/golib/goweb"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
)

type WorkController struct{}

// OPTIONS: /work
func (cr *WorkController) Options(cx *goweb.Context) {
	LogRequest(cx.Request)
	cx.RespondWithOK()
	return
}

// GET: /work/{id}
// get a workunit by id, read-only
func (cr *WorkController) Read(id string, cx *goweb.Context) {
	LogRequest(cx.Request)

	// Gather query params
	query := &Query{Li: cx.Request.URL.Query()}

	if (query.Has("datatoken") || query.Has("privateenv")) && query.Has("client") {
		cg, err := request.AuthenticateClientGroup(cx.Request)
		if err != nil {
			if err.Error() == e.NoAuth || err.Error() == e.UnAuth || err.Error() == e.InvalidAuth {
				if conf.CLIENT_AUTH_REQ == true {
					cx.RespondWithError(http.StatusUnauthorized)
					return
				}
			} else {
				logger.Error("Err@AuthenticateClientGroup: " + err.Error())
				cx.RespondWithError(http.StatusInternalServerError)
				return
			}
		}
		// check that clientgroup auth token matches group of client
		clientid := query.Value("client")
		client, err := core.QMgr.GetClient(clientid)
		if err != nil {
			cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
			return
		}
		if cg != nil && client.Group != cg.Name {
			cx.RespondWithErrorMessage("Clientgroup name in token does not match that in the client configuration.", http.StatusBadRequest)
			return
		}

		if query.Has("datatoken") { //a client is requesting data token for this job
			token, err := core.QMgr.FetchDataToken(id, clientid)
			if err != nil {
				cx.RespondWithErrorMessage("error in getting token for job "+id, http.StatusBadRequest)
				return
			}
			//cx.RespondWithData(token)
			RespondTokenInHeader(cx, token)
			return
		}

		if query.Has("privateenv") { //a client is requesting data token for this job
			envs, err := core.QMgr.FetchPrivateEnv(id, clientid)
			if err != nil {
				cx.RespondWithErrorMessage("error in getting token for job "+id, http.StatusBadRequest)
				return
			}
			//cx.RespondWithData(token)
			RespondPrivateEnvInHeader(cx, envs)
			return
		}
	}

	// Try to authenticate user.
	u, err := request.Authenticate(cx.Request)
	if err != nil && err.Error() != e.NoAuth {
		cx.RespondWithErrorMessage(err.Error(), http.StatusUnauthorized)
		return
	}

	// If no auth was provided, and anonymous read is allowed, use the public user
	if u == nil {
		if conf.ANON_READ == true {
			u = &user.User{Uuid: "public"}
		} else {
			cx.RespondWithErrorMessage(e.NoAuth, http.StatusUnauthorized)
			return
		}
	}

	// Load workunit by id
	workunit, err := core.QMgr.GetWorkById(id)
	if err != nil {
		cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
		return
	}

	jobid, err := core.GetJobIdByWorkId(id)
	if err != nil {
		cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
		return
	}

	job, err := core.LoadJob(jobid)
	if err != nil {
		cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
		return
	}

	// User must have read permissions on job or be job owner or be an admin
	rights := job.Acl.Check(u.Uuid)
	if job.Acl.Owner != u.Uuid && rights["read"] == false && u.Admin == false {
		cx.RespondWithErrorMessage(e.UnAuth, http.StatusUnauthorized)
		return
	}

	if query.Has("report") { //retrieve report: stdout or stderr or worknotes
		reportmsg, err := core.QMgr.GetReportMsg(id, query.Value("report"))
		if err != nil {
			cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
			return
		}
		cx.RespondWithData(reportmsg)
		return
	}

	if err != nil {
		if err.Error() != e.QueueEmpty {
			logger.Error("Err@work_Read:core.QMgr.GetWorkById(): " + err.Error())
		}
		cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
		return
	}
	// Base case respond with workunit in json
	cx.RespondWithData(workunit)
	return
}

// GET: /work
// checkout a workunit with earliest submission time
// to-do: to support more options for workunit checkout
func (cr *WorkController) ReadMany(cx *goweb.Context) {

	// Gather query params
	query := &Query{Li: cx.Request.URL.Query()}

	if !query.Has("client") { //view workunits
		// Try to authenticate user.
		u, err := request.Authenticate(cx.Request)
		if err != nil && err.Error() != e.NoAuth {
			cx.RespondWithErrorMessage(err.Error(), http.StatusUnauthorized)
			return
		}

		// If no auth was provided, and anonymous read is allowed, use the public user
		if u == nil {
			if conf.ANON_READ == true {
				u = &user.User{Uuid: "public"}
			} else {
				cx.RespondWithErrorMessage(e.NoAuth, http.StatusUnauthorized)
				return
			}
		}

		var workunits []*core.Workunit
		if query.Has("state") {
			workunits = core.QMgr.ShowWorkunitsByUser(query.Value("state"), u)
		} else {
			workunits = core.QMgr.ShowWorkunitsByUser("", u)
		}
		cx.RespondWithData(workunits)
		return
	}

	cg, err := request.AuthenticateClientGroup(cx.Request)
	if err != nil {
		if err.Error() == e.NoAuth || err.Error() == e.UnAuth || err.Error() == e.InvalidAuth {
			if conf.CLIENT_AUTH_REQ == true {
				cx.RespondWithError(http.StatusUnauthorized)
				return
			}
		} else {
			logger.Error("Err@AuthenticateClientGroup: " + err.Error())
			cx.RespondWithError(http.StatusInternalServerError)
			return
		}
	}

	// check that clientgroup auth token matches group of client
	clientid := query.Value("client")
	client, err := core.QMgr.GetClient(clientid)
	if err != nil {
		cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
		return
	}
	if cg != nil && client.Group != cg.Name {
		cx.RespondWithErrorMessage("Clientgroup name in token does not match that in the client configuration.", http.StatusBadRequest)
		return
	}

	if core.Service == "proxy" { //drive proxy workStealer to checkout work from server
		core.ProxyWorkChan <- true
	}

	//checkout a workunit in FCFS order
	workunits, err := core.QMgr.CheckoutWorkunits("FCFS", clientid, 1)

	if err != nil {
		if err.Error() != e.QueueEmpty && err.Error() != e.NoEligibleWorkunitFound && err.Error() != e.ClientNotFound && err.Error() != e.ClientSuspended {
			logger.Error("Err@work_ReadMany:core.QMgr.GetWorkByFCFS(): " + err.Error() + ";client=" + clientid)
		}
		cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
		return
	}

	//log access info only when the queue is not empty, save some log
	LogRequest(cx.Request)

	//log event about workunit checkout (WO)
	workids := []string{}
	for _, work := range workunits {
		workids = append(workids, work.Id)
	}

	logger.Event(event.WORK_CHECKOUT,
		"workids="+strings.Join(workids, ","),
		"clientid="+clientid)

	// Base case respond with node in json
	cx.RespondWithData(workunits[0])
	return
}

// PUT: /work/{id} -> status update
func (cr *WorkController) Update(id string, cx *goweb.Context) {
	LogRequest(cx.Request)
	// Gather query params
	query := &Query{Li: cx.Request.URL.Query()}
	if !query.Has("client") {
		cx.RespondWithErrorMessage("This request type requires the client=clientid parameter.", http.StatusBadRequest)
		return
	}

	// Check auth
	cg, err := request.AuthenticateClientGroup(cx.Request)
	if err != nil {
		if err.Error() == e.NoAuth || err.Error() == e.UnAuth || err.Error() == e.InvalidAuth {
			if conf.CLIENT_AUTH_REQ == true {
				cx.RespondWithError(http.StatusUnauthorized)
				return
			}
		} else {
			logger.Error("Err@AuthenticateClientGroup: " + err.Error())
			cx.RespondWithError(http.StatusInternalServerError)
			return
		}
	}

	// check that clientgroup auth token matches group of client
	clientid := query.Value("client")
	client, err := core.QMgr.GetClient(clientid)
	if err != nil {
		cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
		return
	}
	if cg != nil && client.Group != cg.Name {
		cx.RespondWithErrorMessage("Clientgroup name in token does not match that in the client configuration.", http.StatusBadRequest)
		return
	}

	if query.Has("status") && query.Has("client") { //notify execution result: "done" or "fail"
		notice := core.Notice{WorkId: id, Status: query.Value("status"), ClientId: query.Value("client"), Notes: ""}
		if query.Has("computetime") {
			if comptime, err := strconv.Atoi(query.Value("computetime")); err == nil {
				notice.ComputeTime = comptime
			}
		}
		if query.Has("report") { // if "report" is specified in query, parse performance statistics or errlog
			if _, files, err := ParseMultipartForm(cx.Request); err == nil {
				if _, ok := files["perf"]; ok {
					core.QMgr.FinalizeWorkPerf(id, files["perf"].Path)
				}
				if _, ok := files["notes"]; ok {
					if notes, err := ioutil.ReadFile(files["notes"].Path); err == nil {
						notice.Notes = string(notes)
					}
				}
				if _, ok := files["stdout"]; ok {
					core.QMgr.SaveStdLog(id, "stdout", files["stdout"].Path)
				}
				if _, ok := files["stderr"]; ok {
					core.QMgr.SaveStdLog(id, "stderr", files["stderr"].Path)
				}
				if _, ok := files["worknotes"]; ok {
					core.QMgr.SaveStdLog(id, "worknotes", files["worknotes"].Path)
				}
			}
		}
		core.QMgr.NotifyWorkStatus(notice)
	}
	cx.RespondWithData("ok")
	return
}
