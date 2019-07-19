package cwl

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/MG-RAST/AWE/lib/logger"
	"github.com/MG-RAST/AWE/lib/rwmutex"
	"github.com/davecgh/go-spew/spew"
)

// WorkflowContext global object for each job submission
type WorkflowContext struct {
	rwmutex.RWMutex
	GraphDocument `yaml:",inline" json:",inline" bson:",inline" mapstructure:",squash"` // fields: CwlVersion, Base, Graph, Namespaces, Schemas (all interface-based !)
	Path          string
	//Namespaces   map[string]string
	//CWLVersion
	//CwlVersion CWLVersion    `yaml:"cwl_version"  json:"cwl_version" bson:"cwl_version" mapstructure:"cwl_version"`
	//CWL_graph  []interface{} `yaml:"cwl_graph"  json:"cwl_graph" bson:"cwl_graph" mapstructure:"cwl_graph"`
	// old ParsingContext
	IfObjects map[string]interface{} `yaml:"-"  json:"-" bson:"-" mapstructure:"-"` // graph objects
	Objects   map[string]CWLObject   `yaml:"-"  json:"-" bson:"-" mapstructure:"-"` // graph objects , stores all objects (replaces All ???)

	//Workflows          map[string]*Workflow          `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	//InputParameter     map[string]*InputParameter    `yaml:"-"  json:"-" bson:"-" mapstructure:"-"` // WorkflowInput
	//WorkflowStepInputs map[string]*WorkflowStepInput `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	//WorkflowStepInstance map[string]*WorkflowStep `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	//CommandLineTools   map[string]*CommandLineTool   `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	//ExpressionTools    map[string]*ExpressionTool    `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	//Files              map[string]*File              `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	//Strings            map[string]*String            `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	//Ints               map[string]*Int               `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	//Booleans           map[string]*Boolean           `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	All map[string]CWLObject `yaml:"-"  json:"-" bson:"-" mapstructure:"-"` // everything goes in here

	WorkflowCount int `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	//Job_input          *Job_document
	//Job_input_map *JobDocMap `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`

	Schemata    map[string]CWLType_Type `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	Initialized bool                    `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
	Initialzing bool                    `yaml:"-"  json:"-" bson:"-" mapstructure:"-"` // collect objects in ths phase

	Name string `yaml:"-"  json:"-" bson:"-" mapstructure:"-"`
}

func NewWorkflowContext() (context *WorkflowContext) {

	logger.Debug(3, "(NewWorkflowContext) starting")

	context = &WorkflowContext{}
	context.Name = "George"
	return
}

func (context *WorkflowContext) InitBasic() {

	context.RWMutex.Init("context")

	if context.IfObjects == nil {
		context.IfObjects = make(map[string]interface{})
	}

	if context.Objects == nil {
		context.Objects = make(map[string]CWLObject)
	}

	if context.All == nil {
		context.All = make(map[string]CWLObject)
	}

	//if context.WorkflowStepInstance == nil {
	//	context.WorkflowStepInstance = make(map[string]*WorkflowStep)
	//}

	if context.Schemata == nil {
		context.Schemata = make(map[string]CWLType_Type)
	}

	context.WorkflowCount = 0
	return
}

// search for #entrypoint and create objects recursively
func (context *WorkflowContext) Init(entrypoint string) (err error) {

	logger.Debug(3, "(WorkflowContext/Init) start")
	if context.Initialized == true {
		err = fmt.Errorf("(WorkflowContext/Init) already initialized")
		return
	}

	context.InitBasic()

	if context.CwlVersion == "" {
		err = fmt.Errorf("(WorkflowContext/Init) context.CwlVersion ==nil")
		return
	}

	graph := context.GraphDocument.Graph

	if len(graph) == 0 {
		err = fmt.Errorf("(WorkflowContext/Init) len(graph) == 0")
		return
	}

	logger.Debug(3, "(WorkflowContext/Init) len(graph): %d", len(graph))

	// put interface objetcs into map: populate context.If_objects
	for i, _ := range graph {

		//fmt.Printf("graph element type: %s\n", reflect.TypeOf(graph[i]))
		//spew.Dump(graph[i])

		if graph[i] == nil {
			err = fmt.Errorf("(WorkflowContext/Init) graph[i] empty array element")
			return
		}

		var id string
		id, err = GetID(graph[i])
		if err != nil {
			fmt.Println("(WorkflowContext/Init) object without id:")
			spew.Dump(graph[i])
			return
		}
		//fmt.Printf("id=\"%s\\n", id)

		context.IfObjects[id] = graph[i]

	}

	if entrypoint == "" { // for worker
		return
	}

	logger.Debug(3, "(WorkflowContext/Init) len(context.IfObjects): %d", len(context.IfObjects))

	entrypointIf, hasEntrypointObject := context.IfObjects[entrypoint] // e.g. #entrypoint
	if !hasEntrypointObject {

		if len(context.IfObjects) == 1 {
			for key, value := range context.IfObjects {
				entrypoint = key
				entrypointIf = value
			}
		}

		if entrypoint == "" {
			var keys string
			for key := range context.IfObjects {
				keys += "," + key
			}
			err = fmt.Errorf("(WorkflowContext/Init) entrypoint %s not found in graph (found %s)", entrypoint, keys)
			return
		}
	}

	// start with #entrypoint
	// recursivly add objects to context
	context.Initialzing = true
	var object CWLObject
	var schemataNew []CWLType_Type
	object, schemataNew, err = NewCWLObject(entrypointIf, "", nil, context)
	if err != nil {
		fmt.Printf("(WorkflowContext/Init) entrypointIf")
		spew.Dump(entrypointIf)
		err = fmt.Errorf("(WorkflowContext/Init) A NewCWLObject returned %s", err.Error())
		return
	}
	context.Initialzing = false
	context.Objects[entrypoint] = object

	err = context.AddSchemata(schemataNew, true)
	if err != nil {
		err = fmt.Errorf("(WorkflowContext/Init) context.AddSchemata returned %s", err.Error())
		return
	}
	//for i, _ := range schemataNew {
	//	schemata = append(schemata, schemataNew[i])
	//}
	//fmt.Println("context.All")
	//for key, _ := range context.All {
	//	fmt.Printf("context.All: %s\n", key)
	//}
	//panic("done")

	context.GraphDocument.Graph = nil
	context.GraphDocument.Graph = []interface{}{}
	for key, value := range context.Objects {
		logger.Debug(3, "(WorkflowContext/Init) adding %s to context.GraphDocument.Graph", key)
		//err = context.Add(key, value, "WorkflowContext/Init")
		//if err != nil {
		//	err = fmt.Errorf("(WorkflowContext/Init) context.Add( returned %s", err.Error())
		//	return
		//}

		context.GraphDocument.Graph = append(context.GraphDocument.Graph, value)
	}
	//fmt.Println("(WorkflowContext/Init) context.Objects: ")
	//spew.Dump(context.Objects)

	context.Initialized = true
	return
}

func (c *WorkflowContext) Evaluate(raw string) (parsed string) {

	reg := regexp.MustCompile(`\$\([\w.]+\)`) // https://github.com/google/re2/wiki/Syntax

	parsed = raw
	for {

		matches := reg.FindAll([]byte(parsed), -1)
		fmt.Printf("Matches: %d\n", len(matches))
		if len(matches) == 0 {
			return parsed
		}
		for _, match := range matches {
			key := bytes.TrimPrefix(match, []byte("$("))
			key = bytes.TrimSuffix(key, []byte(")"))

			// trimming of inputs. is only a work-around
			key = bytes.TrimPrefix(key, []byte("inputs."))

			value_str := ""
			value, err := c.GetString(string(key))

			if err != nil {
				value_str = "<ERROR_NOT_FOUND:" + string(key) + ">"
			} else {
				value_str = value.String()
			}

			logger.Debug(1, "evaluate %s -> %s\n", key, value_str)
			parsed = strings.Replace(parsed, string(match), value_str, 1)
		}

	}

}

func (c *WorkflowContext) AddSchemata(obj []CWLType_Type, writeLock bool) (err error) {
	//fmt.Printf("(AddSchemata)\n")
	if writeLock {
		err = c.LockNamed("AddSchemata")
		if err != nil {
			return
		}
		defer c.Unlock()
	}

	if c.Schemata == nil {
		c.Schemata = make(map[string]CWLType_Type)
	}

	for i, _ := range obj {
		id := obj[i].GetID()
		if id == "" {
			err = fmt.Errorf("id empty")
			return
		}

		//fmt.Printf("Adding %s\n", id)

		_, ok := c.Schemata[id]
		if ok {
			return
		}

		c.Schemata[id] = obj[i]
	}
	return
}

func (c *WorkflowContext) GetSchemata() (obj []CWLType_Type, err error) {
	obj = []CWLType_Type{}
	for _, schema := range c.Schemata {
		obj = append(obj, schema)
	}
	return
}

func (c *WorkflowContext) AddArray(object_array []NamedCWLObject) (err error) {

	for i, _ := range object_array {
		pair := object_array[i]

		err = c.Add(pair.Id, pair.Value, "AddArray")
		if err != nil {
			return
		}

	}

	return

}

func (c *WorkflowContext) Add(id string, obj CWLObject, caller string) (err error) {

	if id == "" {
		// anonymous objects are not stored
		return
	}

	if !strings.HasPrefix(id, "#") {
		err = fmt.Errorf("(WorkflowContext/Add) id %s is not absolute", id)
		return
	}

	logger.Debug(3, "(WorkflowContext/Add) Adding object %s to collection (type: %s, caller: %s)", id, reflect.TypeOf(obj), caller)

	if c.All == nil {
		c.All = make(map[string]CWLObject)
	}

	_, ok := c.All[id]
	if ok {
		err = fmt.Errorf("(WorkflowContext/Add) Object %s already in collection (caller: %s)", id, caller)
		return
	}

	switch obj.(type) {
	case *Workflow:
		//fmt.Printf("(c.All) c.WorkflowCount: %d\n", c.WorkflowCount)
		c.WorkflowCount += 1
		//fmt.Printf("(c.All) c.WorkflowCount: %d\n", c.WorkflowCount)
		msg := fmt.Sprintf("(WorkflowContext/Add) new WorkflowCount: %d (context: %p, caller: %s, name: %s)", c.WorkflowCount, &c, caller, c.Name)
		logger.Debug(3, msg)
		//fmt.Printf("(c.All) msg: %s\n", msg)
		//for i, _ := range c.All {
		//	fmt.Println(i)
		//}

	//	c.Workflows[id] = obj.(*Workflow)
	case *WorkflowStepInput:
		obj_real, ok := obj.(*WorkflowStepInput)
		if !ok {
			err = fmt.Errorf("could not make WorkflowStepInput type assertion")
			return
		}
		c.All[id] = obj_real
	case *CommandLineTool:
		obj_real, ok := obj.(*CommandLineTool)
		if !ok {
			err = fmt.Errorf("could not make CommandLineTool type assertion")
			return
		}
		c.All[id] = obj_real
	case *ExpressionTool:
		obj_real, ok := obj.(*ExpressionTool)
		if !ok {
			err = fmt.Errorf("could not make ExpressionTool type assertion")
			return
		}
		c.All[id] = obj_real
	case *File:
		obj_real, ok := obj.(*File)
		if !ok {
			err = fmt.Errorf("could not make File type assertion")
			return
		}
		c.All[id] = obj_real
	case *String:
		obj_real, ok := obj.(*String)
		if !ok {
			err = fmt.Errorf("could not make String type assertion")
			return
		}
		c.All[id] = obj_real
	case *Boolean:
		obj_real, ok := obj.(*Boolean)
		if !ok {
			err = fmt.Errorf("could not make Boolean type assertion")
			return
		}
		c.All[id] = obj_real
	case *Int:
		obj_int, ok := obj.(*Int)
		if !ok {
			err = fmt.Errorf("could not make Int type assertion")
			return
		}
		c.All[id] = obj_int
	default:
		logger.Debug(3, "adding type %s to WorkflowContext.All", reflect.TypeOf(obj))
	}

	c.All[id] = obj
	//fmt.Printf("(c.All) after insertion of %s (caller: %s)\n", id, caller)
	//for i, _ := range c.All {
	//	fmt.Println(i)
	//}
	return
}

func (c *WorkflowContext) Get(id string, do_read_lock bool) (obj CWLObject, ok bool, err error) {

	if do_read_lock {
		var read_lock rwmutex.ReadLock
		read_lock, err = c.RLockNamed("WorkflowContext/Get")
		if err != nil {
			return
		}
		defer c.RUnlockNamed(read_lock)
	}

	obj, ok = c.All[id]
	if !ok {
		logger.Debug(3, "(WorkflowContext/Get) did not find: %s", id)
		for k, _ := range c.All {
			logger.Debug(3, "(WorkflowContext/Get) available: %s", k)
		}
		//err = fmt.Errorf("(All) item %s not found in collection", id)
	}

	return
}

func (c *WorkflowContext) GetType(id string) (obj_type string, err error) {
	do_read_lock := true
	if do_read_lock {
		read_lock, xerr := c.RLockNamed("WorkflowContext/Get")
		if xerr != nil {
			err = xerr
			return
		}
		defer c.RUnlockNamed(read_lock)
	}
	var ok bool
	var obj CWLObject
	obj, ok = c.All[id]
	if !ok {
		err = fmt.Errorf("(GetCWLTypeType) Object %s not found in All", id)
		return
	}

	obj_type = fmt.Sprintf("%s", reflect.TypeOf(obj))

	return

}

// func (c *WorkflowContext) GetCWLType(id string) (obj CWLType, err error) {
// 	var ok bool
// 	obj, ok = c.Files[id]
// 	if ok {
// 		return
// 	}
// 	obj, ok = c.Strings[id]
// 	if ok {
// 		return
// 	}

// 	obj, ok = c.Ints[id]
// 	if ok {
// 		return
// 	}
// 	obj, ok = c.Booleans[id]
// 	if ok {
// 		return
// 	}

// 	err = fmt.Errorf("(GetType) %s not found", id)
// 	return

// }

func (c *WorkflowContext) GetFile(id string) (obj *File, err error) {
	var obj_generic CWLObject
	var ok bool
	obj_generic, ok, err = c.Get(id, true)
	if err != nil {
		err = fmt.Errorf("(GetFile) error getting item %s: %s", id, err.Error())
		return
	}
	if !ok {
		err = fmt.Errorf("(GetFile) item %s not found in collection: %s", id, err.Error())
		return
	}

	obj, ok = obj_generic.(*File)
	if !ok {
		err = fmt.Errorf("(GetFile) Item %s has wrong type: %s", id, reflect.TypeOf(obj_generic))
	}
	return
}

func (c *WorkflowContext) GetString(id string) (obj *String, err error) {
	var obj_generic CWLObject
	var ok bool
	obj_generic, ok, err = c.Get(id, true)
	if err != nil {
		err = fmt.Errorf("(GetString) error getting item %s: %s", id, err.Error())
		return
	}
	if !ok {
		err = fmt.Errorf("(GetString) item %s not found in collection: %s", id, err.Error())
		return
	}

	obj, ok = obj_generic.(*String)
	if !ok {
		err = fmt.Errorf("(GetString) Item %s has wrong type: %s", id, reflect.TypeOf(obj_generic))
	}
	return
}

func (c *WorkflowContext) GetInt(id string) (obj *Int, err error) {
	var obj_generic CWLObject
	var ok bool
	obj_generic, ok, err = c.Get(id, true)
	if err != nil {
		err = fmt.Errorf("(GetInt) error getting item %s: %s", id, err.Error())
		return
	}
	if !ok {
		err = fmt.Errorf("(GetInt) item %s not found in collection: %s", id, err.Error())
		return
	}

	obj, ok = obj_generic.(*Int)
	if !ok {
		err = fmt.Errorf("(GetInt) Item %s has wrong type: %s", id, reflect.TypeOf(obj_generic))
	}
	return
}

func (c *WorkflowContext) GetWorkflowStepInput(id string) (obj *WorkflowStepInput, err error) {
	var obj_generic CWLObject
	var ok bool
	obj_generic, ok, err = c.Get(id, true)
	if err != nil {
		err = fmt.Errorf("(GetWorkflowStepInput) error getting item %s: %s", id, err.Error())
		return
	}
	if !ok {
		err = fmt.Errorf("(GetWorkflowStepInput) item %s not found in collection: %s", id, err.Error())
		return
	}

	obj, ok = obj_generic.(*WorkflowStepInput)
	if !ok {
		err = fmt.Errorf("(GetWorkflowStepInput) Item %s has wrong type: %s", id, reflect.TypeOf(obj_generic))
	}
	return
}

func (c *WorkflowContext) GetCommandLineTool(id string) (obj *CommandLineTool, err error) {
	var obj_generic CWLObject
	var ok bool
	obj_generic, ok, err = c.Get(id, true)
	if err != nil {
		err = fmt.Errorf("(GetCommandLineTool) error getting item %s: %s", id, err.Error())
		return
	}
	if !ok {
		err = fmt.Errorf("(GetCommandLineTool) item %s not found in collection: %s", id, err.Error())
		return
	}

	obj, ok = obj_generic.(*CommandLineTool)
	if !ok {
		err = fmt.Errorf("(GetCommandLineTool) Item %s has wrong type: %s", id, reflect.TypeOf(obj_generic))
	}
	return
}

func (c *WorkflowContext) GetExpressionTool(id string) (obj *ExpressionTool, err error) {
	var obj_generic CWLObject
	var ok bool
	obj_generic, ok, err = c.Get(id, true)
	if err != nil {
		err = fmt.Errorf("(GetExpressionTool) error getting item %s: %s", id, err.Error())
		return
	}
	if !ok {
		err = fmt.Errorf("(GetExpressionTool) item %s not found in collection: %s", id, err.Error())
		return
	}

	obj, ok = obj_generic.(*ExpressionTool)
	if !ok {
		err = fmt.Errorf("(GetExpressionTool) Item %s has wrong type: %s", id, reflect.TypeOf(obj_generic))
	}
	return
}

func (c *WorkflowContext) GetWorkflow(id string) (obj *Workflow, err error) {
	var obj_generic CWLObject
	var ok bool
	obj_generic, ok, err = c.Get(id, true)
	if err != nil {
		err = fmt.Errorf("(GetWorkflow) error getting item %s: %s", id, err.Error())
		return
	}
	if !ok {

		keys := ""
		for key := range c.All {
			keys += "," + key
		}

		err = fmt.Errorf("(GetWorkflow) item %s not found in collection (found: %s)", id, keys)
		return
	}

	obj, ok = obj_generic.(*Workflow)
	if !ok {
		err = fmt.Errorf("(GetWorkflow) Item %s has wrong type: %s", id, reflect.TypeOf(obj_generic))
	}
	return
}
