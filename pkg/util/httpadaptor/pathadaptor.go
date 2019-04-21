package httpadaptor

import (
	"regexp"
	"strings"

	"github.com/megaease/easegateway/pkg/logger"
)

type (
	// pathAdaptorSpec describes rules for adapting path.
	pathAdaptorSpec struct {
		Replace       string         `yaml:"replace"`
		AddPrefix     string         `yaml:"addPrefix"`
		TrimPrefix    string         `yaml:"trimPrefix"`
		RegexpReplace *RegexpReplace `yaml:"regexpReplace"`
	}

	// RegexpReplace use regexp-replace pair to rewrite path.
	RegexpReplace struct {
		Regexp  string `yaml:"regexp" v:"required,regexp"`
		Replace string `yaml:"replace" v:"required"`

		re *regexp.Regexp
	}

	pathAdaptor struct {
		spec *pathAdaptorSpec
	}
)

// newPathAdaptor creates a pathAdaptor.
func newPathAdaptor(spec *pathAdaptorSpec) *pathAdaptor {
	if spec.RegexpReplace != nil {
		var err error
		spec.RegexpReplace.re, err = regexp.Compile(spec.RegexpReplace.Regexp)
		if err != nil {
			logger.Errorf("BUG: compile regexp %s failed: %v",
				spec.RegexpReplace.Regexp, err)
		}
	}

	return &pathAdaptor{
		spec: spec,
	}
}

// Adapt adapts path.
func (a *pathAdaptor) Adapt(path string) string {
	if len(a.spec.Replace) != 0 {
		return a.spec.Replace
	}

	if len(a.spec.AddPrefix) != 0 {
		return a.spec.AddPrefix + path
	}

	if len(a.spec.TrimPrefix) != 0 {
		return strings.TrimPrefix(path, a.spec.TrimPrefix)
	}

	if a.spec.RegexpReplace != nil && a.spec.RegexpReplace.re != nil {
		return a.spec.RegexpReplace.re.ReplaceAllString(path,
			a.spec.RegexpReplace.Replace)
	}

	return path
}
