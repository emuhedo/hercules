package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gogo/protobuf/proto"
	"github.com/sergi/go-diff/diffmatchpatch"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/utils/merkletrie"
	"gopkg.in/src-d/hercules.v4"
)

// ChurnAnalysis contains the intermediate state which is mutated by Consume(). It should implement
// hercules.LeafPipelineItem.
type ChurnAnalysis struct {
	// No special merge logic is required
	hercules.NoopMerger
	// Process each merge only once
	hercules.OneShotMergeProcessor
	TrackPeople bool

	global []editInfo
	people map[int][]editInfo

	// references IdentityDetector.ReversedPeopleDict
	reversedPeopleDict []string
}

type editInfo struct {
	Day     int
	Added   int
	Removed int
}

// ChurnAnalysisResult is returned by Finalize() and represents the analysis result.
type ChurnAnalysisResult struct {
	Global Edits
	People map[string]Edits
}

type Edits struct {
	Days      []int
	Additions []int
	Removals  []int
}

const (
	ConfigChurnTrackPeople = "Churn.TrackPeople"
)

// Analysis' name in the graph is usually the same as the type's name, however, does not have to.
func (churn *ChurnAnalysis) Name() string {
	return "ChurnAnalysis"
}

// LeafPipelineItem-s normally do not act as intermediate nodes and thus we return an empty slice.
func (churn *ChurnAnalysis) Provides() []string {
	return []string{}
}

// Requires returns the list of dependencies which must be supplied in Consume().
// file_diff - line diff for each commit change
// changes - list of changed files for each commit
// blob_cache - set of blobs affected by each commit
// day - number of days since start for each commit
// author - author of the commit
func (churn *ChurnAnalysis) Requires() []string {
	arr := [...]string{
		hercules.DependencyFileDiff,
		hercules.DependencyTreeChanges,
		hercules.DependencyBlobCache,
		hercules.DependencyDay,
		hercules.DependencyAuthor}
	return arr[:]
}

// ListConfigurationOptions tells the engine which parameters can be changed through the command
// line.
func (churn *ChurnAnalysis) ListConfigurationOptions() []hercules.ConfigurationOption {
	opts := [...]hercules.ConfigurationOption{{
		Name:        ConfigChurnTrackPeople,
		Description: "Record detailed statistics per each developer.",
		Flag:        "churn-people",
		Type:        hercules.BoolConfigurationOption,
		Default:     false},
	}
	return opts[:]
}

// Flag returns the command line switch which activates the analysis.
func (churn *ChurnAnalysis) Flag() string {
	return "churn"
}

// Description returns the text which explains what the analysis is doing.
func (churn *ChurnAnalysis) Description() string {
	return "Collects the daily numbers of inserted and removed lines."
}

// Configure applies the parameters specified in the command line. Map keys correspond to "Name".
func (churn *ChurnAnalysis) Configure(facts map[string]interface{}) {
	if val, exists := facts[ConfigChurnTrackPeople].(bool); exists {
		churn.TrackPeople = val
	}
	if churn.TrackPeople {
		churn.reversedPeopleDict = facts[hercules.FactIdentityDetectorReversedPeopleDict].([]string)
	}
}

// Initialize resets the internal temporary data structures and prepares the object for Consume().
func (churn *ChurnAnalysis) Initialize(repository *git.Repository) {
	churn.global = []editInfo{}
	churn.people = map[int][]editInfo{}
	churn.OneShotMergeProcessor.Initialize()
}

func (churn *ChurnAnalysis) Consume(deps map[string]interface{}) (map[string]interface{}, error) {
	if !churn.ShouldConsumeCommit(deps) {
		return nil, nil
	}
	fileDiffs := deps[hercules.DependencyFileDiff].(map[string]hercules.FileDiffData)
	treeDiffs := deps[hercules.DependencyTreeChanges].(object.Changes)
	cache := deps[hercules.DependencyBlobCache].(map[plumbing.Hash]*object.Blob)
	day := deps[hercules.DependencyDay].(int)
	author := deps[hercules.DependencyAuthor].(int)
	for _, change := range treeDiffs {
		action, err := change.Action()
		if err != nil {
			return nil, err
		}
		added := 0
		removed := 0
		switch action {
		case merkletrie.Insert:
			added, err = hercules.CountLines(cache[change.To.TreeEntry.Hash])
			if err != nil && err.Error() == "binary" {
				err = nil
			}
		case merkletrie.Delete:
			removed, err = hercules.CountLines(cache[change.From.TreeEntry.Hash])
			if err != nil && err.Error() == "binary" {
				err = nil
			}
		case merkletrie.Modify:
			diffs := fileDiffs[change.To.Name]
			for _, edit := range diffs.Diffs {
				length := utf8.RuneCountInString(edit.Text)
				switch edit.Type {
				case diffmatchpatch.DiffEqual:
					continue
				case diffmatchpatch.DiffInsert:
					added += length
				case diffmatchpatch.DiffDelete:
					removed += length
				}
			}

		}
		if err != nil {
			return nil, err
		}
		ei := editInfo{Day: day, Added: added, Removed: removed}
		churn.global = append(churn.global, ei)
		if churn.TrackPeople {
			seq, exists := churn.people[author]
			if !exists {
				seq = []editInfo{}
			}
			seq = append(seq, ei)
			churn.people[author] = seq
		}
	}
	return nil, nil
}

// Fork clones the same item several times on branches.
func (churn *ChurnAnalysis) Fork(n int) []hercules.PipelineItem {
	return hercules.ForkSamePipelineItem(churn, n)
}

func (churn *ChurnAnalysis) Finalize() interface{} {
	result := ChurnAnalysisResult{
		Global: editInfosToEdits(churn.global),
		People: map[string]Edits{},
	}
	if churn.TrackPeople {
		for key, val := range churn.people {
			result.People[churn.reversedPeopleDict[key]] = editInfosToEdits(val)
		}
	}
	return result
}

func (churn *ChurnAnalysis) Serialize(result interface{}, binary bool, writer io.Writer) error {
	burndownResult := result.(ChurnAnalysisResult)
	if binary {
		return churn.serializeBinary(&burndownResult, writer)
	}
	churn.serializeText(&burndownResult, writer)
	return nil
}

func (churn *ChurnAnalysis) serializeText(result *ChurnAnalysisResult, writer io.Writer) {
	fmt.Fprintln(writer, "  global:")
	printEdits(result.Global, writer, 4)
	for key, val := range result.People {
		fmt.Fprintf(writer, "  %s:\n", hercules.SafeYamlString(key))
		printEdits(val, writer, 4)
	}
}

func (churn *ChurnAnalysis) serializeBinary(result *ChurnAnalysisResult, writer io.Writer) error {
	message := ChurnAnalysisResultMessage{
		Global: editsToEditsMessage(result.Global),
		People: map[string]*EditsMessage{},
	}
	for key, val := range result.People {
		message.People[key] = editsToEditsMessage(val)
	}
	serialized, err := proto.Marshal(&message)
	if err != nil {
		return err
	}
	writer.Write(serialized)
	return nil
}

func editInfosToEdits(eis []editInfo) Edits {
	aux := map[int]*editInfo{}
	for _, ei := range eis {
		ptr := aux[ei.Day]
		if ptr == nil {
			ptr = &editInfo{Day: ei.Day}
		}
		ptr.Added += ei.Added
		ptr.Removed += ei.Removed
		aux[ei.Day] = ptr
	}
	seq := []int{}
	for key := range aux {
		seq = append(seq, key)
	}
	sort.Ints(seq)
	edits := Edits{
		Days:      make([]int, len(seq)),
		Additions: make([]int, len(seq)),
		Removals:  make([]int, len(seq)),
	}
	for i, day := range seq {
		edits.Days[i] = day
		edits.Additions[i] = aux[day].Added
		edits.Removals[i] = aux[day].Removed
	}
	return edits
}

func printEdits(edits Edits, writer io.Writer, indent int) {
	strIndent := strings.Repeat(" ", indent)
	printArray := func(arr []int, name string) {
		fmt.Fprintf(writer, "%s%s: [", strIndent, name)
		for i, v := range arr {
			if i < len(arr)-1 {
				fmt.Fprintf(writer, "%d, ", v)
			} else {
				fmt.Fprintf(writer, "%d]\n", v)
			}
		}
	}
	printArray(edits.Days, "days")
	printArray(edits.Additions, "additions")
	printArray(edits.Removals, "removals")
}

func editsToEditsMessage(edits Edits) *EditsMessage {
	message := &EditsMessage{
		Days:      make([]uint32, len(edits.Days)),
		Additions: make([]uint32, len(edits.Additions)),
		Removals:  make([]uint32, len(edits.Removals)),
	}
	copyInts := func(arr []int, where []uint32) {
		for i, v := range arr {
			where[i] = uint32(v)
		}
	}
	copyInts(edits.Days, message.Days)
	copyInts(edits.Additions, message.Additions)
	copyInts(edits.Removals, message.Removals)
	return message
}

func init() {
	hercules.Registry.Register(&ChurnAnalysis{})
}
