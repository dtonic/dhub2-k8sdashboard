//go:build e2efixture

package e2efixture

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 테스트 전용 패키지(e2efixture·testcluster)가 프로덕션 코드로 새어 들어가면 안 됩니다.
// cmd/e2efixture는 빌드 태그 뒤에 있어야 하고, 그 외 어떤 비테스트 파일도
// 이 패키지들을 임포트하면 안 됩니다.
func TestFixtureStaysOutOfProduction(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("호출 위치를 알 수 없습니다")
	}
	moduleRoot := filepath.Dir(filepath.Dir(filepath.Dir(self))) // apps/api
	if filepath.Base(moduleRoot) != "api" {
		t.Fatalf("module root가 apps/api로 한정되지 않았습니다: %s", moduleRoot)
	}
	goMod, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil || !strings.Contains(string(goMod), "module github.com/xenx96/k8s-dashboard/apps/api") {
		t.Fatalf("apps/api module root를 증명하지 못했습니다: %v", err)
	}

	const (
		fixtureImport     = "github.com/xenx96/k8s-dashboard/apps/api/internal/e2efixture"
		testclusterImport = "github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
	)

	fset := token.NewFileSet()
	err = filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		relPath, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relPath)
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		isTagged := strings.Contains(string(src), "//go:build e2efixture")
		// 픽스처 소스는 **전부**(내부 패키지·cmd 포함) 빌드 태그 뒤에 있어야 합니다.
		// 일반 `go build ./...`에는 한 파일도 포함되지 않습니다.
		if strings.HasPrefix(rel, "internal/e2efixture/") || strings.HasPrefix(rel, "cmd/e2efixture/") {
			if !isTagged {
				t.Errorf("%s: e2efixture 소스에 //go:build e2efixture 태그가 없습니다", rel)
			}
			return nil
		}
		// testcluster는 일반 테스트가 임포트하는 공용 픽스처라 태그가 없습니다.
		// 대신 아래 임포트 검사로 프로덕션 유입을 막습니다.
		// _test.go는 어차피 프로덕션 바이너리에 실리지 않으므로 임포트 검사 대상이 아닙니다.
		if strings.Contains(rel, "internal/testcluster") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			v := strings.Trim(imp.Path.Value, `"`)
			if v == fixtureImport || v == testclusterImport {
				t.Errorf("%s: 프로덕션 코드가 테스트 전용 패키지 %s를 임포트합니다", rel, v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
