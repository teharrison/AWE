package controller

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MG-RAST/AWE/lib/conf"
	"github.com/MG-RAST/AWE/lib/core"
	e "github.com/MG-RAST/AWE/lib/errors"
	"github.com/MG-RAST/AWE/lib/logger"
	"github.com/MG-RAST/AWE/lib/request"
	"github.com/MG-RAST/AWE/lib/user"
	"github.com/MG-RAST/golib/goweb"
	"gopkg.in/mgo.v2/bson"
)

var (
	logo = "\n" +
		" +--------------+  +----+   +----+   +----+  +--------------+\n" +
		" |              |  |    |   |    |   |    |  |              |\n" +
		" |    +----+    |  |    |   |    |   |    |  |    +---------+\n" +
		" |    |    |    |  |    |   |    |   |    |  |    |          \n" +
		" |    +----+    |  |    |   |    |   |    |  |    +---------+\n" +
		" |              |  |    |   |    |   |    |  |              |\n" +
		" |    +----+    |  |    |   |    |   |    |  |    +---------+\n" +
		" |    |    |    |  |    \\---/    \\---/    |  |    |          \n" +
		" |    |    |    |  |                      |  |    +---------+\n" +
		" |    |    |    |   \\        /---\\       /   |              |\n" +
		" +----+    +----+     \\-----/     \\-----/    +--------------+\n"
)

type StandardResponse struct {
	S int         `json:"status"`
	D interface{} `json:"data"`
	E []string    `json:"error"`
}

func PrintLogo() {
	fmt.Println(logo)
	return
}

type Query struct {
	Li map[string][]string
}

func (q *Query) Has(key string) bool {
	if _, has := q.Li[key]; has {
		return true
	}
	return false
}

func (q *Query) Value(key string) string {
	return q.Li[key][0]
}

func (q *Query) List(key string) []string {
	return q.Li[key]
}

func (q *Query) All() map[string][]string {
	return q.Li
}

func (q *Query) Empty() bool {
	if len(q.Li) == 0 {
		return true
	}
	return false
}

func LogRequest(req *http.Request) {
	host, _, _ := net.SplitHostPort(req.RemoteAddr)
	//	prefix := fmt.Sprintf("%s [%s]", host, time.Now().Format(time.RFC1123))
	suffix := ""
	if _, auth := req.Header["Authorization"]; auth {
		suffix = "AUTH"
	}
	url := ""
	if req.URL.RawQuery != "" {
		url = fmt.Sprintf("%s %s?%s", req.Method, req.URL.Path, req.URL.RawQuery)
	} else {
		url = fmt.Sprintf("%s %s", req.Method, req.URL.Path)
	}
	logger.Log.Access(host + " \"" + url + suffix + "\"")
}

func RawDir(cx *goweb.Context) {
	LogRequest(cx.Request)
	http.ServeFile(cx.ResponseWriter, cx.Request, fmt.Sprintf("%s/%s", conf.DATA_PATH, cx.Request.URL.Path))
}

func SiteDir(cx *goweb.Context) {
	LogRequest(cx.Request)
	if cx.Request.URL.Path == "/" {
		http.ServeFile(cx.ResponseWriter, cx.Request, conf.SITE_PATH+"/main.html")
	} else {
		http.ServeFile(cx.ResponseWriter, cx.Request, conf.SITE_PATH+cx.Request.URL.Path)
	}
}

const (
	longDateForm = "2006-01-02T15:04:05-07:00"
)

type anonymous struct {
	Read   bool `json:"read"`
	Write  bool `json:"write"`
	Delete bool `json:"delete"`
}

type resource struct {
	R []string `json:"resources"`
	F []string `json:"info_indexes"`
	U string   `json:"url"`
	//D             string    `json:"documentation"`
	Title string    `json:"title"` // title to show in AWE monitor
	C     string    `json:"contact"`
	I     string    `json:"id"`
	O     []string  `json:"auth"`
	P     anonymous `json:"anonymous_permissions"`
	T     string    `json:"type"`
	S     string    `json:"queue_status"`
	V     string    `json:"version"`
	Time  string    `json:"server_time"`
	//GitCommitHash string    `json:"git_commit_hash"`
	Uptime       string `json:"uptime"`
	InstanceUUID string `json:"uuid"`
}

func ResourceDescription(cx *goweb.Context) {
	LogRequest(cx.Request)

	if cx.Request.URL.Path != "/" {
		cx.RespondWithErrorMessage(fmt.Sprintf("Resource %s unknown", cx.Request.URL.Path), http.StatusBadRequest)
		return
	}

	anonPerms := new(anonymous)
	anonPerms.Read = conf.ANON_READ
	anonPerms.Write = conf.ANON_WRITE
	anonPerms.Delete = conf.ANON_DELETE

	var auth []string
	if conf.GLOBUS_TOKEN_URL != "" && conf.GLOBUS_PROFILE_URL != "" {
		auth = append(auth, "globus")
	}
	if len(conf.AUTH_OAUTH) > 0 {
		for b := range conf.AUTH_OAUTH {
			auth = append(auth, b)
		}
	}

	r := resource{
		R: []string{},
		F: core.JobInfoIndexes,
		U: apiUrl(cx) + "/",
		//D:     siteUrl(cx) + "/",
		Title: conf.TITLE,
		C:     conf.ADMIN_EMAIL,
		I:     "AWE",
		O:     auth,
		P:     *anonPerms,
		T:     core.Service,
		S:     core.QMgr.QueueStatus(),
		V:     conf.VERSION,
		Time:  time.Now().Format(longDateForm),
		//GitCommitHash: conf.GIT_COMMIT_HASH,
		Uptime:       time.Since(core.StartTime).String(),
		InstanceUUID: core.ServerUUID,
	}

	if core.Service == "server" {
		r.R = []string{"job", "work", "client", "queue", "awf", "event"}
	} else if core.Service == "proxy" {
		r.R = []string{"client", "work"}
	}

	cx.WriteResponse(r, 200)
	return
}

func apiUrl(cx *goweb.Context) string {
	if conf.API_URL != "" {
		return conf.API_URL
	}
	return "http://" + cx.Request.Host
}

// deprecated
func siteUrl(cx *goweb.Context) string {
	if conf.SITE_URL != "" {
		return conf.SITE_URL
	} else if strings.Contains(cx.Request.Host, ":") {
		return fmt.Sprintf("http://%s:%d", strings.Split(cx.Request.Host, ":")[0], conf.SITE_PORT)
	}
	return "http://" + cx.Request.Host
}

// helper function for create & update
func ParseMultipartForm(r *http.Request) (params map[string]string, files core.FormFiles, err error) {
	params = make(map[string]string)
	files = make(core.FormFiles)

	reader, xerr := r.MultipartReader()
	if xerr != nil {
		err = fmt.Errorf("(ParseMultipartForm) MultipartReader not created: %s", xerr.Error())
		return
	}
	for {
		var part *multipart.Part
		part, err = reader.NextPart()
		if err != nil {
			if err == io.EOF {
				err = nil
				break
			}
			err = fmt.Errorf("(ParseMultipartForm) reader.NextPart() error: %s", err.Error())
			return
		}

		//fmt.Printf("(ParseMultipartForm) part: \"%s\" \n", part.FileName())
		//spew.Dump(part)

		if part.FileName() == "" {
			//fmt.Printf("(ParseMultipartForm) filename empty\n")
			buffer := make([]byte, 32*1024)
			n, err := part.Read(buffer)
			//fmt.Printf("(ParseMultipartForm) n=%d\n", n)
			//if n == 0 {
			//	break
			//}
			if err != nil {
				//fmt.Printf("(ParseMultipartForm) error: %s\n", err.Error())
				if err != io.EOF {
					err = fmt.Errorf("(ParseMultipartForm) part.Read(buffer) error: %s", err.Error())
					return nil, nil, err
				}

				if err == io.EOF { // inidicates end, butr you should use buffer
					err = nil

				}

			}
			//fmt.Printf("(ParseMultipartForm) no error\n")
			buf_len := 50
			if n < 50 {
				buf_len = n
			}
			logger.Debug(3, "FormName: %s Content: %s", part.FormName(), buffer[0:buf_len])
			//fmt.Printf("(ParseMultipartForm) writing param: %s\n", buffer[0:n])
			params[part.FormName()] = fmt.Sprintf("%s", buffer[0:n])
		} else {
			//fmt.Printf("(ParseMultipartForm) found filename")
			tmpPath := fmt.Sprintf("%s/temp/%d%d", conf.DATA_PATH, rand.Int(), rand.Int())
			//logger.Debug(3, "FormName: %s tmpPath: %s", part.FormName(), tmpPath)
			files[part.FormName()] = core.FormFile{Name: part.FileName(), Path: tmpPath, Checksum: make(map[string]string)}
			bytes_written := 0
			var tmpFile *os.File
			tmpFile, err = os.Create(tmpPath)
			if err != nil {
				err = fmt.Errorf("(ParseMultipartForm) os.Create(tmpPath) error: %s", err.Error())
				return nil, nil, err
			}

			last_loop := false
			buffer := make([]byte, 32*1024)
			for {
				n := 0

				n, err = part.Read(buffer)
				//logger.Debug(3, "read from part: %d", n)
				if err != nil {
					//logger.Debug(3, "err != nil")
					if err == io.EOF {
						err = nil
						last_loop = true
					} else {
						err = fmt.Errorf("part.Read(buffer) error: %s", err.Error())
						return
					}

				}
				//logger.Debug(3, "after reading.... n: %d", n)
				if n == 0 {
					break
				}
				bytes_written += n
				//logger.Debug(3, "after reading, bytes_written: %d", bytes_written)
				m := 0
				m, err = tmpFile.Write(buffer[0:n])
				if err != nil {
					err = fmt.Errorf("(ParseMultipartForm) tmpFile.Write error: %s", err.Error())
					return
				}
				if m != n {
					err = fmt.Errorf("(ParseMultipartForm) m != n ")
					return
				}
				if last_loop {
					break
				}
			}
			tmpFile.Close()

			//logger.Debug(3, "FormName: %s bytes_written: %d", part.FormName(), bytes_written)
		}

	}

	return
}

func RespondTokenInHeader(cx *goweb.Context, token string) {
	cx.ResponseWriter.Header().Set("Datatoken", token)
	cx.Respond(nil, http.StatusOK, nil, cx)
	return
}

func RespondPrivateEnvInHeader(cx *goweb.Context, Envs map[string]string) (err error) {
	env_stream, err := json.Marshal(Envs)
	if err != nil {
		return err
	}
	cx.ResponseWriter.Header().Set("Privateenv", string(env_stream[:]))
	cx.Respond(nil, http.StatusOK, nil, cx)
	return
}

func GetAuthorizedUser(cx *goweb.Context) (u *user.User, done bool) {
	// Try to authenticate user.

	done = false

	u, err := request.Authenticate(cx.Request)
	if err != nil && err.Error() != e.NoAuth {
		cx.RespondWithErrorMessage(err.Error(), http.StatusUnauthorized)
		done = true
		return
	}

	// If no auth was provided, and anonymous read is allowed, use the public user
	if u == nil {
		if conf.ANON_WRITE == true {
			u = &user.User{Uuid: "public"}
		} else {
			cx.RespondWithErrorMessage(e.NoAuth, http.StatusUnauthorized)
			done = true
			return
		}
	}
	return
}

func GetClientGroup(cx *goweb.Context) (cg *core.ClientGroup, done bool) {
	done = false
	cg, err := request.AuthenticateClientGroup(cx.Request)
	if err != nil {
		if err.Error() == e.NoAuth || err.Error() == e.UnAuth || err.Error() == e.InvalidAuth {
			if conf.CLIENT_AUTH_REQ == true {
				cx.RespondWithError(http.StatusUnauthorized)
				done = true
				return
			}
		} else {
			logger.Error("Err@AuthenticateClientGroup: " + err.Error())
			cx.RespondWithError(http.StatusInternalServerError)
			done = true
			return
		}
	}
	return
}

func DecodeBase64(cx *goweb.Context, id string) (return_id string) {
	if strings.HasPrefix(id, "base64:") {
		id_b64 := strings.TrimPrefix(id, "base64:")
		id_bytes, err := base64.StdEncoding.DecodeString(id_b64)
		if err != nil {
			cx.RespondWithErrorMessage("error decoding base64 workunit identifier: "+id, http.StatusBadRequest)
			return
		}

		return_id = string(id_bytes[:])
	} else {
		return_id = id
	}

	return
}

func contains(list []string, elem string) bool {
	for _, t := range list {
		if t == elem {
			return true
		}
	}
	return false
}

func GetAclQuery(u *user.User) (query bson.M) {

	query = bson.M{}
	if u.Uuid == "public" {
		query["acl.read"] = "public"
	} else {
		query["$or"] = []bson.M{bson.M{"acl.read": "public"}, bson.M{"acl.read": u.Uuid}, bson.M{"acl.owner": u.Uuid}, bson.M{"acl": bson.M{"$exists": "false"}}}
	}
	return
}

func QueryParseDefaultOptions(cx *goweb.Context) (opt *core.DefaultQueryOptions, err error) {

	query := &Query{Li: cx.Request.URL.Query()}

	opt = &core.DefaultQueryOptions{}

	opt.Limit = conf.DEFAULT_PAGE_SIZE
	opt.Offset = 0
	opt.Sort = make([]string, 0)

	if query.Has("limit") {
		opt.Limit, err = strconv.Atoi(query.Value("limit"))
		if err != nil {
			cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
			return
		}
	}
	if query.Has("offset") {
		opt.Offset, err = strconv.Atoi(query.Value("offset"))
		if err != nil {
			cx.RespondWithErrorMessage(err.Error(), http.StatusBadRequest)
			return
		}
	}

	if query.Has("order") {
		var sort_str string
		sort_str = query.Value("order")

		if sort_str != "" {
			sort_array := strings.Split(sort_str, ",")
			opt.Sort = sort_array
		}

	}
	return
}
