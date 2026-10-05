package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// The packaged crash helper drives the real destruction path with its JSON
// inventory as the whole physical world. Every seam the in-process destruction
// installer (installRuntimeDeletionCandidateSeams) replaces asks tmux a
// question, so the helper must replace each one as well, and restore it. A
// seam left real consults whatever server the helper's environment names: the
// foreign-server guard, for one, lists the default server $TMUX points at.
func TestRuntimeLifecycle_CrashHelperStubsEveryDestructionSeam(t *testing.T) {
	installer := seamAssignmentsForTest(t, "runtime_delete_test.go", "installRuntimeDeletionCandidateSeams")
	helper := seamAssignmentsForTest(t, "runtime_lifecycle_helper_bridge.go", "RunRuntimeLifecycleCrashHelper")
	if len(installer) == 0 {
		t.Fatal("found no seams in installRuntimeDeletionCandidateSeams")
	}
	var missing []string
	for seam := range installer {
		// One assignment stubs the seam and one restores it.
		if helper[seam] < 2 {
			missing = append(missing, seam)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Fatalf("RunRuntimeLifecycleCrashHelper does not stub and restore %s, which the in-process destruction seams replace",
			strings.Join(missing, ", "))
	}
}

// seamAssignmentsForTest counts plain assignments to package-level seams
// (identifiers ending in Fn) anywhere inside function, closures included.
func seamAssignmentsForTest(t *testing.T, file, function string) map[string]int {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	found := false
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != function {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok || assign.Tok != token.ASSIGN {
				return true
			}
			for _, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && strings.HasSuffix(ident.Name, "Fn") {
					counts[ident.Name]++
				}
			}
			return true
		})
	}
	if !found {
		t.Fatalf("%s has no function %s", file, function)
	}
	return counts
}
