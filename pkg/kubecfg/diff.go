// Copyright 2017 The kubecfg authors
//
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package kubecfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"

	isatty "github.com/mattn/go-isatty"
	"github.com/sergi/go-diff/diffmatchpatch"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/bitnami/kubecfg/utils"
)

var ErrDiffFound = fmt.Errorf("Differences found.")

// Matches all the line starts on a diff text, which is where we put diff markers and indent
var DiffLineStart = regexp.MustCompile("(^|\n)(.)")

var DiffKeyValue = regexp.MustCompile(`"([-._a-zA-Z0-9]+)":\s"([[:alnum:]=+]+)",?`)

// DiffCmd represents the diff subcommand
type DiffCmd struct {
	Client           dynamic.Interface
	Mapper           meta.RESTMapper
	DefaultNamespace string
	OmitSecrets      bool
	OmitSame         bool

	DiffStrategy string
}

// difference is one of the possible states of a comparison between a local and remote object
type difference int64

const (
	// No difference between local and remote object
	differenceNone difference = iota
	// Could not get difference due to an error
	differenceError
	// Object does not exist on remote but exists locally
	differenceOnlyLocal
	// Object exists both remote and locally, but is diffirent
	differenceDifferent
)

// diffRes is the result of a comparison of a local vs. remote object.
type diffRes struct {
	// The name of the resource that was compared.
	desc string
	// The difference type.
	diff difference
	// If difference is differenceDifferent, the text diff of the two objects.
	details string
	// If difference is differenceError, the error that occured when performing the comparison.
	err error
}

// diffOne performs a comparison of a local object vs an object on the
// DiffCmd's Client remote.
func (c DiffCmd) diffOne(obj *unstructured.Unstructured) *diffRes {
	dmp := diffmatchpatch.New()

	desc := fmt.Sprintf("%s %s", utils.ResourceNameFor(c.Mapper, obj), utils.FqName(obj))

	fatal := func(err error) *diffRes {
		return &diffRes{
			desc: desc,
			diff: differenceError,
			err:  err,
		}
	}
	result := func(diff difference, details string) *diffRes {
		return &diffRes{
			desc:    desc,
			diff:    diff,
			details: details,
		}
	}

	log.Debug("Fetching ", desc)
	client, err := utils.ClientForResource(c.Client, c.Mapper, obj, c.DefaultNamespace)
	if err != nil {
		return fatal(err)
	}

	if obj.GetName() == "" {
		return fatal(fmt.Errorf("Error fetching one of the %s: it does not have a name set", utils.ResourceNameFor(c.Mapper, obj)))
	}

	liveObj, err := client.Get(obj.GetName(), metav1.GetOptions{})
	if err != nil && errors.IsNotFound(err) {
		log.Debugf("%s doesn't exist on the server", desc)
		liveObj = nil
	} else if err != nil {
		return fatal(fmt.Errorf("Error fetching %s: %v", desc, err))
	}

	if liveObj == nil {
		return result(differenceOnlyLocal, "")
	}

	liveObjObject := liveObj.Object
	if c.DiffStrategy == "subset" {
		liveObjObject = removeMapFields(obj.Object, liveObjObject)
	}

	liveObjText, _ := json.MarshalIndent(liveObjObject, "", "  ")
	objText, _ := json.MarshalIndent(obj.Object, "", "  ")

	liveObjTextLines, objTextLines, lines := dmp.DiffLinesToChars(string(liveObjText), string(objText))

	diff := dmp.DiffMain(
		string(liveObjTextLines),
		string(objTextLines),
		false)

	diff = dmp.DiffCharsToLines(diff, lines)
	if (len(diff) == 1) && (diff[0].Type == diffmatchpatch.DiffEqual) {
		return result(differenceNone, "")
	}

	details := c.formatDiff(diff, isatty.IsTerminal(os.Stdout.Fd()), c.OmitSecrets && obj.GetKind() == "Secret")
	return result(differenceDifferent, details)
}

func (c DiffCmd) Run(apiObjects []*unstructured.Unstructured, out io.Writer) error {
	sort.Sort(utils.AlphabeticalOrder(apiObjects))

	diffFound := false
	for _, obj := range apiObjects {
		o := c.diffOne(obj)

		printObjectHeader := func() {
			fmt.Fprintln(out, "---")
			fmt.Fprintf(out, "- live %s\n+ config %s\n", o.desc, o.desc)
		}

		switch o.diff {
		case differenceNone:
			if !c.OmitSame {
				printObjectHeader()
			}

		case differenceError:
			fmt.Fprintf(out, "%v\n", o.err)

		case differenceOnlyLocal:
			diffFound = true
			printObjectHeader()
			fmt.Fprintf(out, "%s doesn't exist on server\n", o.desc)

		case differenceDifferent:
			diffFound = true
			printObjectHeader()
			fmt.Fprintf(out, "%s\n", o.details)
		}
	}

	if diffFound {
		return ErrDiffFound
	}
	return nil
}

// Formats the supplied Diff as a unified-diff-like text with infinite context and optionally colorizes it.
func (c DiffCmd) formatDiff(diffs []diffmatchpatch.Diff, color bool, omitchanges bool) string {
	var buff bytes.Buffer

	for _, diff := range diffs {
		text := diff.Text

		if omitchanges {
			text = DiffKeyValue.ReplaceAllString(text, "$1: <omitted>")
		}
		switch diff.Type {
		case diffmatchpatch.DiffInsert:
			if color {
				_, _ = buff.WriteString("\x1b[32m")
			}
			_, _ = buff.WriteString(DiffLineStart.ReplaceAllString(text, "$1+ $2"))
			if color {
				_, _ = buff.WriteString("\x1b[0m")
			}
		case diffmatchpatch.DiffDelete:
			if color {
				_, _ = buff.WriteString("\x1b[31m")
			}
			_, _ = buff.WriteString(DiffLineStart.ReplaceAllString(text, "$1- $2"))
			if color {
				_, _ = buff.WriteString("\x1b[0m")
			}
		case diffmatchpatch.DiffEqual:
			if !omitchanges {
				_, _ = buff.WriteString(DiffLineStart.ReplaceAllString(text, "$1  $2"))
			}
		}
	}

	return buff.String()
}

// See also feature request for golang reflect pkg at
func isEmptyValue(i interface{}) bool {
	switch v := i.(type) {
	case []interface{}:
		return len(v) == 0
	case []string:
		return len(v) == 0
	case map[string]interface{}:
		return len(v) == 0
	case bool:
		return !v
	case float64:
		return v == 0
	case int64:
		return v == 0
	case string:
		return v == ""
	case nil:
		return true
	default:
		panic(fmt.Sprintf("Found unexpected type %T in json unmarshal (value=%v)", i, i))
	}
}

func removeFields(config, live interface{}) interface{} {
	switch c := config.(type) {
	case map[string]interface{}:
		if live, ok := live.(map[string]interface{}); ok {
			return removeMapFields(c, live)
		}
	case []interface{}:
		if live, ok := live.([]interface{}); ok {
			return removeListFields(c, live)
		}
	}
	return live
}

func removeMapFields(config, live map[string]interface{}) map[string]interface{} {
	result := map[string]interface{}{}
	for k, v1 := range config {
		v2, ok := live[k]
		if !ok {
			// Copy empty value from config, as API won't return them,
			// see https://github.com/bitnami/kubecfg/issues/179
			if isEmptyValue(v1) {
				result[k] = v1
			}
			continue
		}
		result[k] = removeFields(v1, v2)
	}
	return result
}

func removeListFields(config, live []interface{}) []interface{} {
	// If live is longer than config, then the extra elements at the end of the
	// list will be returned as is so they appear in the diff.
	result := make([]interface{}, 0, len(live))
	for i, v2 := range live {
		if len(config) > i {
			result = append(result, removeFields(config[i], v2))
		} else {
			result = append(result, v2)
		}
	}
	return result
}

func istty(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		return isatty.IsTerminal(f.Fd())
	}
	return false
}
