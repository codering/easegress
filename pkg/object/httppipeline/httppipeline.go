package httppipeline

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/megaease/easegateway/pkg/context"
	"github.com/megaease/easegateway/pkg/logger"
	"github.com/megaease/easegateway/pkg/object/httpserver"
	"github.com/megaease/easegateway/pkg/scheduler"
	"github.com/megaease/easegateway/pkg/util/stringtool"
	"github.com/megaease/easegateway/pkg/v"

	yaml "gopkg.in/yaml.v2"
)

const (
	// Kind is HTTPPipeline kind.
	Kind = "HTTPPipeline"

	// LabelEND is the built-in label for jumping of flow.
	LabelEND = "END"
)

func init() {
	scheduler.Register(&scheduler.ObjectRecord{
		Kind:              Kind,
		DefaultSpecFunc:   DefaultSpec,
		NewFunc:           New,
		DependObjectKinds: []string{httpserver.Kind},
	})
}

type (
	// HTTPPipeline is Object HTTPPipeline.
	HTTPPipeline struct {
		spec *Spec

		handlers       *sync.Map
		runningPlugins []*runningPlugin
	}

	runningPlugin struct {
		spec   map[string]interface{}
		jumpIf map[string]string
		plugin Plugin
		meta   *PluginMeta
		pr     *PluginRecord
	}

	// Spec describes the HTTPPipeline.
	Spec struct {
		scheduler.ObjectMeta `yaml:",inline"`

		Flow    []Flow                   `yaml:"flow" jsonschema:"omitempty"`
		Plugins []map[string]interface{} `yaml:"plugins" jsonschema:"-"`
	}

	// Flow controls the flow of pipeline.
	Flow struct {
		Plugin string            `yaml:"plugin" jsonschema:"required,format=urlname"`
		JumpIf map[string]string `yaml:"jumpIf" jsonschema:"omitempty"`
	}

	// Status contains all status gernerated by runtime, for displaying to users.
	Status struct {
		Timestamp int64 `yaml:"timestamp"`

		Health string `yaml:"health"`

		Plugins map[string]interface{} `yaml:"plugins"`
	}

	// PipelineContext contains the context of the HTTPPipeline.
	PipelineContext struct {
		PluginStats []*PluginStat
	}

	// PluginStat records the statistics of the running plugin.
	PluginStat struct {
		Name     string
		Kind     string
		Result   string
		Duration time.Duration
	}
)

func (ps *PluginStat) log() string {
	result := ps.Result
	if result != "" {
		result += ","
	}
	return stringtool.Cat(ps.Name, "(", result, ps.Duration.String(), ")")
}

func (ctx *PipelineContext) log() string {
	if len(ctx.PluginStats) == 0 {
		return "<empty>"
	}

	logs := make([]string, len(ctx.PluginStats))
	for i, pluginStat := range ctx.PluginStats {
		logs[i] = pluginStat.log()
	}

	return strings.Join(logs, "->")
}

var (
	// context.HTTPContext: *PipelineContext
	runningContexts sync.Map = sync.Map{}
)

func newAndSetPipelineContext(ctx context.HTTPContext) *PipelineContext {
	pipeCtx := &PipelineContext{}

	runningContexts.Store(ctx, pipeCtx)

	return pipeCtx
}

// GetPipelineContext returns the corresponding PipelineContext of the HTTPContext,
// and a bool flag to represent it succeed or not.
func GetPipelineContext(ctx context.HTTPContext) (*PipelineContext, bool) {
	value, ok := runningContexts.Load(ctx)
	if !ok {
		return nil, false
	}

	pipeCtx, ok := value.(*PipelineContext)
	if !ok {
		logger.Errorf("BUG: want *PipelineContext, got %T", value)
		return nil, false
	}

	return pipeCtx, true
}

func deletePipelineContext(ctx context.HTTPContext) {
	runningContexts.Delete(ctx)
}

// DefaultSpec returns HTTPPipeline default spec.
func DefaultSpec() *Spec {
	return &Spec{}
}

func marshal(i interface{}) []byte {
	buff, err := yaml.Marshal(i)
	if err != nil {
		panic(fmt.Errorf("marsharl %#v failed: %v", i, err))
	}
	return buff
}

func unmarshal(buff []byte, i interface{}) {
	err := yaml.Unmarshal(buff, i)
	if err != nil {
		panic(fmt.Errorf("unmarshal failed: %v", err))
	}
}

func extractPluginsData(config []byte) interface{} {
	var whole map[string]interface{}
	unmarshal(config, &whole)
	return whole["plugins"]
}

func convertToPluginBuffs(obj interface{}) map[string][]byte {
	var plugins []map[string]interface{}
	unmarshal(marshal(obj), &plugins)

	rst := make(map[string][]byte)
	for _, p := range plugins {
		buff := marshal(p)
		meta := &PluginMeta{}
		unmarshal(buff, meta)
		rst[meta.Name] = buff
	}
	return rst
}

func validatePluginMeta(meta *PluginMeta) error {
	if len(meta.Name) == 0 {
		return fmt.Errorf("plugin name is required")
	}
	if len(meta.Kind) == 0 {
		return fmt.Errorf("plugin kind is required")
	}

	return nil
}

// Validate validates Spec.
func (s Spec) Validate(config []byte) (err error) {
	errPrefix := "plugins"
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s: %s", errPrefix, r)
		}
	}()

	pluginsData := extractPluginsData(config)
	if pluginsData == nil {
		return fmt.Errorf("validate failed: plugins is required")
	}
	pluginBuffs := convertToPluginBuffs(pluginsData)

	pluginRecords := make(map[string]*PluginRecord)
	for _, plugin := range s.Plugins {
		buff := marshal(plugin)

		meta := &PluginMeta{}
		unmarshal(buff, meta)
		err := validatePluginMeta(meta)
		if err != nil {
			panic(err)
		}
		if meta.Name == LabelEND {
			panic(fmt.Errorf("can't use %s(built-in label) for plugin name", LabelEND))
		}

		if _, exists := pluginRecords[meta.Name]; exists {
			panic(fmt.Errorf("conflict name: %s", meta.Name))
		}

		pr, exists := pluginBook[meta.Kind]
		if !exists {
			panic(fmt.Errorf("plugins: unsuppoted kind %s", meta.Kind))
		}
		pluginRecords[meta.Name] = pr

		pluginSpec := reflect.ValueOf(pr.DefaultSpecFunc).Call(nil)[0].Interface()
		unmarshal(buff, pluginSpec)
		vr := v.Validate(pluginSpec, pluginBuffs[meta.Name])
		if !vr.Valid() {
			panic(vr)
		}
		err = nil
		if pr == nil {
			panic(fmt.Errorf("plugin kind %s not found", plugin["kind"]))
		}
	}

	errPrefix = "flow"

	plugins := make(map[string]struct{})
	for _, f := range s.Flow {
		if _, exists := plugins[f.Plugin]; exists {
			panic(fmt.Errorf("repeated plugin %s", f.Plugin))
		}
	}

	labelsValid := map[string]struct{}{LabelEND: struct{}{}}
	for i := len(s.Flow) - 1; i >= 0; i-- {
		f := s.Flow[i]
		pr, exists := pluginRecords[f.Plugin]
		if !exists {
			panic(fmt.Errorf("plugin %s not found", f.Plugin))
		}
		for result, label := range f.JumpIf {
			if !stringtool.StrInSlice(result, pr.Results) {
				panic(fmt.Errorf("plugin %s: result %s is not in %v",
					f.Plugin, result, pr.Results))
			}
			if _, exists := labelsValid[label]; !exists {
				panic(fmt.Errorf("plugin %s: label %s not found",
					f.Plugin, label))
			}
		}
		labelsValid[f.Plugin] = struct{}{}
	}

	return nil
}

// New creates an HTTPPipeline
func New(spec *Spec, prev *HTTPPipeline, handlers *sync.Map) (tmp *HTTPPipeline) {
	hp := &HTTPPipeline{
		spec:     spec,
		handlers: handlers,
	}

	runningPlugins := make([]*runningPlugin, 0)
	if len(spec.Flow) == 0 {
		for _, pluginSpec := range spec.Plugins {
			runningPlugins = append(runningPlugins, &runningPlugin{
				spec: pluginSpec,
			})
		}
	} else {
		for _, f := range spec.Flow {
			var pluginSpec map[string]interface{}
			for _, ps := range spec.Plugins {
				buff := marshal(ps)
				meta := &PluginMeta{}
				unmarshal(buff, meta)
				if meta.Name == f.Plugin {
					pluginSpec = ps
					break
				}
			}
			if pluginSpec == nil {
				panic(fmt.Errorf("flow plugin %s not found in plugins", f.Plugin))
			}
			runningPlugins = append(runningPlugins, &runningPlugin{
				spec:   pluginSpec,
				jumpIf: f.JumpIf,
			})
		}
	}

	for _, runningPlugin := range runningPlugins {
		buff := marshal(runningPlugin.spec)

		meta := &PluginMeta{}
		unmarshal(buff, meta)

		pr, exists := pluginBook[meta.Kind]
		if !exists {
			panic(fmt.Errorf("kind %s not found", meta.Kind))
		}

		defaultPluginSpec := reflect.ValueOf(pr.DefaultSpecFunc).Call(nil)[0].Interface()
		unmarshal(buff, defaultPluginSpec)

		prevValue := reflect.New(pr.PluginType).Elem()
		if prev != nil {
			prevPlugin := prev.getRunningPlugin(meta.Name)
			if prevPlugin != nil {
				prevValue = reflect.ValueOf(prevPlugin.plugin)
			}
		}
		in := []reflect.Value{reflect.ValueOf(defaultPluginSpec), prevValue}

		plugin := reflect.ValueOf(pr.NewFunc).Call(in)[0].Interface().(Plugin)

		runningPlugin.plugin, runningPlugin.meta, runningPlugin.pr = plugin, meta, pr
	}

	hp.runningPlugins = runningPlugins

	if prev != nil {
		for _, runningPlugin := range prev.runningPlugins {
			if hp.getRunningPlugin(runningPlugin.meta.Name) == nil {
				runningPlugin.plugin.Close()
			}
		}
	}

	hp.handlers.Store(spec.Name, hp)

	return hp
}

// Handle handles all incoming traffic.
func (hp *HTTPPipeline) Handle(ctx context.HTTPContext) {
	pipeCtx := newAndSetPipelineContext(ctx)
	defer deletePipelineContext(ctx)

	nextPluginName := hp.runningPlugins[0].meta.Name
	for i := 0; i < len(hp.runningPlugins); i++ {
		if nextPluginName == LabelEND {
			break
		}

		runningPlugin := hp.runningPlugins[i]
		if nextPluginName != runningPlugin.meta.Name {
			continue
		}

		startTime := time.Now()
		result := runningPlugin.plugin.Handle(ctx)
		handleDuration := time.Now().Sub(startTime)

		pluginStat := &PluginStat{
			Name:     runningPlugin.meta.Name,
			Kind:     runningPlugin.meta.Kind,
			Result:   result,
			Duration: handleDuration,
		}
		pipeCtx.PluginStats = append(pipeCtx.PluginStats, pluginStat)

		if result != "" {
			if !stringtool.StrInSlice(result, runningPlugin.pr.Results) {
				logger.Errorf("BUG: invalid result %s not in %v",
					result, runningPlugin.pr.Results)
			}

			jumpIf := runningPlugin.jumpIf
			if len(jumpIf) == 0 {
				break
			}
			var exists bool
			nextPluginName, exists = jumpIf[result]
			if !exists {
				break
			}
		} else if i < len(hp.runningPlugins)-1 {
			nextPluginName = hp.runningPlugins[i+1].meta.Name
		}
	}

	ctx.AddTag(stringtool.Cat("pipeline: ", pipeCtx.log()))
}

func (hp *HTTPPipeline) getRunningPlugin(name string) *runningPlugin {
	for _, plugin := range hp.runningPlugins {
		if plugin.meta.Name == name {
			return plugin
		}
	}

	return nil
}

// Status returns Status genreated by Runtime.
// NOTE: Caller must not call Status while Newing.
func (hp *HTTPPipeline) Status() *Status {
	s := &Status{
		Plugins: make(map[string]interface{}),
	}

	for _, runningPlugin := range hp.runningPlugins {
		pluginStatus := reflect.ValueOf(runningPlugin.plugin).
			MethodByName("Status").Call(nil)[0].Interface()
		s.Plugins[runningPlugin.meta.Name] = pluginStatus
	}

	return s
}

// Close closes HTTPPipeline.
func (hp *HTTPPipeline) Close() {
	hp.handlers.Delete(hp.spec.Name)
	for _, runningPlugin := range hp.runningPlugins {
		runningPlugin.plugin.Close()
	}
}
