package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
	"github.com/google/go-jsonnet/formatter"
	"github.com/jdbaldry/go-language-server-protocol/jsonrpc2"
	"github.com/jdbaldry/go-language-server-protocol/lsp/protocol"
)

const (
	symbolTagDefinition protocol.SymbolTag = 100
)

var (
	// errRegexp matches the various Jsonnet location formats in errors.
	// file:line msg
	// file:line:col-endCol msg
	// file:(line:endLine)-(col:endCol) msg
	// Has 10 matching groups.
	errRegexp = regexp.MustCompile(`/.*:(?:(\d+)|(?:(\d+):(\d+)-(\d+))|(?:\((\d+):(\d+)\)-\((\d+):(\d+))\))\s(.*)`)
)

// newServer returns a new language server.
func newServer(client protocol.ClientCloser) (*server, error) {
	vm := jsonnet.MakeVM()
	importer := &jsonnet.FileImporter{JPaths: filepath.SplitList(os.Getenv("JSONNET_PATH"))}
	vm.Importer(importer)
	return &server{
		cache:  newCache(),
		client: client,
		vm:     vm,
	}, nil
}

type server struct {
	cache  *cache
	client protocol.ClientCloser
	vm     *jsonnet.VM
}

func (s *server) CodeAction(context.Context, *protocol.CodeActionParams) ([]protocol.CodeAction, error) {
	return nil, notImplemented("CodeAction")
}

func (s *server) CodeLens(context.Context, *protocol.CodeLensParams) ([]protocol.CodeLens, error) {
	return nil, notImplemented("CodeLens")
}

func (s *server) CodeLensRefresh(context.Context) error {
	return notImplemented("CodeLensRefresh")
}

func (s *server) ColorPresentation(context.Context, *protocol.ColorPresentationParams) ([]protocol.ColorPresentation, error) {
	return nil, notImplemented("ColorPresentation")
}

func (s *server) Completion(context.Context, *protocol.CompletionParams) (*protocol.CompletionList, error) {
	// TODO: This is not implemented but I was unable to disable the capability.
	return nil, nil
}

func (s *server) Declaration(context.Context, *protocol.DeclarationParams) (protocol.Declaration, error) {
	return nil, notImplemented("Declaration")
}

func isDefinition(s protocol.DocumentSymbol) bool {
	for _, t := range s.Tags {
		if t == symbolTagDefinition {
			return true
		}
	}
	return false
}

func (s *server) Definition(ctx context.Context, params *protocol.DefinitionParams) (protocol.Definition, error) {
	doc, err := s.cache.get(params.TextDocument.URI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Definition: unable to get document from cache: %v\n", err)
		return nil, err
	}

	var aux func([]protocol.DocumentSymbol, protocol.DocumentSymbol) (protocol.Definition, error)
	aux = func(stack []protocol.DocumentSymbol, symbol protocol.DocumentSymbol) (protocol.Definition, error) {
		for i, s := range stack {
			if i != 0 {
				fmt.Fprint(os.Stderr, ", ")
			}
			fmt.Fprintf(os.Stderr, "(%s) %s", s.Kind, s.Name)
		}
		fmt.Fprintln(os.Stderr)
		if symbol.Range.Start.Line == params.Position.Line &&
			symbol.Range.Start.Character <= params.Position.Character &&
			symbol.Range.End.Character >= params.Position.Character {

			if symbol.Name == "super" {
				// super can only be used in the right hand side object of a binary `+` operation.
				// The definition the "super" is referring to would be the left hand side.
				// Simplified stack:
				// + lhs obj ... field x super
				prev := stack[len(stack)-1]
				for len(stack) != 0 {
					symbol := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					if symbol.Kind == protocol.Operator {
						return protocol.Definition{{
							URI:   doc.item.URI,
							Range: prev.SelectionRange,
						}}, nil
					}
					prev = symbol
				}
			}
			if symbol.Kind == protocol.File {
				foundAt, err := s.vm.ResolveImport(doc.item.URI.SpanURI().Filename(), symbol.Name)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Definition: unable to resolve import: %v\n", err)
					return nil, err
				}
				return protocol.Definition{{URI: "file:///" + protocol.DocumentURI(foundAt)}}, nil
			}

			if symbol.Kind == protocol.Variable {
				if !isDefinition(symbol) {
					// Found the symbol at point which, the definition of which is the first definition
					// with the same symbol name that we find going back through the stack.
					want := symbol
					for len(stack) != 0 {
						symbol := stack[len(stack)-1]
						stack = stack[:len(stack)-1]
						if symbol.Kind == protocol.Variable && symbol.Name == want.Name && isDefinition(symbol) {
							return protocol.Definition{{
								URI:   doc.item.URI,
								Range: symbol.SelectionRange,
							}}, nil
						}
					}
				}
			}
		}
		stack = append(stack, symbol.Children...)
		for i := len(symbol.Children); i != 0; i-- {
			definition, err := aux(stack, symbol.Children[i-1])
			if definition != nil || err != nil {
				return definition, err
			}
			stack = stack[:len(stack)-1]
		}
		return nil, nil
	}
	return aux([]protocol.DocumentSymbol{doc.symbols}, doc.symbols)
}

func (s *server) Diagnostic(context.Context, *string) (*string, error) {
	return nil, notImplemented("Diagnostic")
}

func (s *server) DiagnosticRefresh(context.Context) error {
	return notImplemented("DiagnosticRefresh")
}

func (s *server) DiagnosticWorkspace(context.Context, *protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
	return nil, notImplemented("DiagnosticWorkspace")
}

func (s *server) publishDiagnostics(uri protocol.DocumentURI) {
	diags := []protocol.Diagnostic{}
	doc, err := s.cache.get(uri)
	if err != nil {
		panic("unable to get document from cache")
	}

	diag := protocol.Diagnostic{Source: "jsonnet evaluation"}
	// Initialize with 1 because we indiscriminately subtract one to map error ranges to LSP ranges.
	line, col, endLine, endCol := 1, 1, 1, 1
	if doc.err != nil {
		lines := strings.Split(doc.err.Error(), "\n")
		if len(lines) == 0 {
			panic("expected at least two lines of Jsonnet evaluation error output")
		}

		var match []string
		runtimeErr := strings.HasPrefix(lines[0], "RUNTIME ERROR:")
		if runtimeErr {
			match = errRegexp.FindStringSubmatch(lines[1])
		} else {
			match = errRegexp.FindStringSubmatch(lines[0])
		}
		if len(match) == 10 {
			if match[1] != "" {
				line, _ = strconv.Atoi(match[1])
				endLine = line + 1
			}
			if match[2] != "" {
				line, _ = strconv.Atoi(match[2])
				col, _ = strconv.Atoi(match[3])
				endLine = line
				endCol, _ = strconv.Atoi(match[4])
			}
			if match[5] != "" {
				line, _ = strconv.Atoi(match[5])
				col, _ = strconv.Atoi(match[6])
				endLine, _ = strconv.Atoi(match[7])
				endCol, _ = strconv.Atoi(match[8])
			}
		}

		if runtimeErr {
			diag.Message = doc.err.Error()
			diag.Severity = protocol.SeverityWarning
		} else {
			diag.Message = match[9]
			diag.Severity = protocol.SeverityError
		}

		diag.Range = protocol.Range{
			Start: protocol.Position{Line: uint32(line - 1), Character: uint32(col - 1)},
			End:   protocol.Position{Line: uint32(endLine - 1), Character: uint32(endCol - 1)},
		}
		diags = append(diags, diag)
	}

	// TODO: Not ignore error.
	// TODO: Fix context.
	_ = s.client.PublishDiagnostics(context.TODO(), &protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: diags,
	})
}

func (s *server) DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	doc, err := s.cache.get(params.TextDocument.URI)
	if err != nil {
		return fmt.Errorf("DidChange: %w", err)
	}

	defer s.publishDiagnostics(params.TextDocument.URI)

	if params.TextDocument.Version > doc.item.Version && len(params.ContentChanges) != 0 {
		doc.item.Text = params.ContentChanges[len(params.ContentChanges)-1].Text
		doc.ast, doc.err = jsonnet.SnippetToAST(doc.item.URI.SpanURI().Filename(), doc.item.Text)
		if doc.err != nil {
			return s.cache.put(doc)
		}
		symbols := analyseSymbols(doc.ast)
		if len(symbols) != 1 {
			panic("There should only be a single root symbol for an AST")
		}
		doc.symbols = symbols[0]
		// TODO: Work out better way to invalidate the VM cache.
		s.vm.Importer(&jsonnet.FileImporter{})
		// TODO: Would the raw AST be better?
		doc.val, doc.err = s.vm.EvaluateAnonymousSnippet(doc.item.URI.SpanURI().Filename(), doc.item.Text)
		return s.cache.put(doc)
	}
	return nil
}

func (s *server) DidChangeConfiguration(context.Context, *protocol.DidChangeConfigurationParams) error {
	return notImplemented("DidChangeConfiguration")
}

func (s *server) DidChangeWatchedFiles(context.Context, *protocol.DidChangeWatchedFilesParams) error {
	return notImplemented("DidChangeWatchedFiles")
}

func (s *server) DidChangeWorkspaceFolders(context.Context, *protocol.DidChangeWorkspaceFoldersParams) error {
	return notImplemented("DidChangeWorkspaceFolders")
}

func (s *server) DidClose(context.Context, *protocol.DidCloseTextDocumentParams) error {
	return notImplemented("DidClose")
}

func (s *server) DidCreateFiles(context.Context, *protocol.CreateFilesParams) error {
	return notImplemented("DidCreateFiles")
}

func (s *server) DidDeleteFiles(context.Context, *protocol.DeleteFilesParams) error {
	return notImplemented("DidDeleteFiles")
}

func analyseSymbols(n ast.Node) (symbols []protocol.DocumentSymbol) {
	switch n := n.(type) {

	case *ast.Array:
		children := []protocol.DocumentSymbol{}
		for _, elem := range n.Elements {
			children = append(children, analyseSymbols(elem.Expr)...)
		}
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: "array",
			Kind: protocol.Array,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			Children: children,
		})

	case *ast.Binary:
		children := analyseSymbols(n.Left)
		children = append(children, analyseSymbols(n.Right)...)
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: n.Op.String(),
			Kind: protocol.Operator,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			Children: children,
		})

	case *ast.DesugaredObject:
		fields := make([]protocol.DocumentSymbol, len(n.Fields))
		locals := make([]protocol.DocumentSymbol, len(n.Locals))
		for i, bind := range n.Locals {
			locals[i] = protocol.DocumentSymbol{
				Name: string(bind.Variable),
				Kind: protocol.Variable,
				Range: protocol.Range{
					Start: protocol.Position{Line: uint32(bind.LocRange.Begin.Line - 1), Character: uint32(bind.LocRange.Begin.Column - 1)},
					End:   protocol.Position{Line: uint32(bind.LocRange.End.Line - 1), Character: uint32(bind.LocRange.End.Column - 1)},
				},
				SelectionRange: protocol.Range{
					Start: protocol.Position{Line: uint32(bind.LocRange.Begin.Line - 1), Character: uint32(bind.LocRange.Begin.Column - 1)},
					End:   protocol.Position{Line: uint32(bind.LocRange.End.Line - 1), Character: uint32(bind.LocRange.End.Column - 1)},
				},
				Tags:     []protocol.SymbolTag{symbolTagDefinition},
				Children: analyseSymbols(bind.Body),
			}
		}
		for i, field := range n.Fields {
			fields[i] = protocol.DocumentSymbol{
				Name: "field",
				Kind: protocol.Field,
				Range: protocol.Range{
					Start: protocol.Position{Line: uint32(field.LocRange.Begin.Line - 1), Character: uint32(field.LocRange.Begin.Column - 1)},
					End:   protocol.Position{Line: uint32(field.LocRange.End.Line - 1), Character: uint32(field.LocRange.End.Column - 1)},
				},
				SelectionRange: protocol.Range{
					Start: protocol.Position{Line: uint32(field.LocRange.Begin.Line - 1), Character: uint32(field.LocRange.Begin.Column - 1)},
					End:   protocol.Position{Line: uint32(field.LocRange.End.Line - 1), Character: uint32(field.LocRange.End.Column - 1)},
				},
				Children: append(analyseSymbols(field.Name), analyseSymbols(field.Body)...),
			}
		}
		symbols = append(symbols, protocol.DocumentSymbol{
			Name:       "object",
			Kind:       protocol.Object,
			Tags:       []protocol.SymbolTag{},
			Deprecated: false,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			Children: append(locals, fields...),
		})

	case *ast.Import:
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: n.File.Value,
			Kind: protocol.File,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
		})

	case *ast.ImportStr:
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: n.File.Value,
			Kind: protocol.File,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
		})
	case *ast.Index:
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: "index",
			Kind: protocol.Field,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			Children: append(analyseSymbols(n.Target), analyseSymbols(n.Index)...),
			Tags:     []protocol.SymbolTag{symbolTagDefinition},
		})

	case *ast.LiteralBoolean:
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: fmt.Sprint(n.Value),
			Kind: protocol.Boolean,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
		})

	case *ast.LiteralNull:
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: "null",
			Kind: protocol.Null,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
		})

	case *ast.LiteralNumber:
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: n.OriginalString,
			Kind: protocol.Number,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
		})

	case *ast.LiteralString:
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: n.Value,
			Kind: protocol.String,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
		})

	case *ast.Local:
		binds := make([]protocol.DocumentSymbol, len(n.Binds))
		for i, bind := range n.Binds {
			binds[i] = protocol.DocumentSymbol{
				Name: string(bind.Variable),
				Kind: protocol.Variable,
				Range: protocol.Range{
					Start: protocol.Position{Line: uint32(bind.LocRange.Begin.Line - 1), Character: uint32(bind.LocRange.Begin.Column - 1)},
					End:   protocol.Position{Line: uint32(bind.LocRange.End.Line - 1), Character: uint32(bind.LocRange.End.Column - 1)},
				},
				SelectionRange: protocol.Range{
					Start: protocol.Position{Line: uint32(bind.LocRange.Begin.Line - 1), Character: uint32(bind.LocRange.Begin.Column - 1)},
					End:   protocol.Position{Line: uint32(bind.LocRange.End.Line - 1), Character: uint32(bind.LocRange.End.Column - 1)},
				},
				Children: analyseSymbols(bind.Body),
				Tags:     []protocol.SymbolTag{symbolTagDefinition},
			}
		}
		symbols = append(symbols, protocol.DocumentSymbol{
			Name:       "local",
			Kind:       protocol.Namespace,
			Tags:       []protocol.SymbolTag{},
			Deprecated: false,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			Children: append(binds, analyseSymbols(n.Body)...),
		})

	case *ast.Self:
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: "self",
			Kind: protocol.Variable,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			Tags: []protocol.SymbolTag{symbolTagDefinition},
		})

	case *ast.SuperIndex:
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: "super",
			Kind: protocol.Field,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			Children: analyseSymbols(n.Index),
			Tags:     []protocol.SymbolTag{symbolTagDefinition},
		})

	case *ast.Var:
		symbols = append(symbols, protocol.DocumentSymbol{
			Name: string(n.Id),
			Kind: protocol.Variable,
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(n.Loc().Begin.Line - 1), Character: uint32(n.Loc().Begin.Column - 1)},
				End:   protocol.Position{Line: uint32(n.Loc().End.Line - 1), Character: uint32(n.Loc().End.Column - 1)},
			},
		})

	default:
		fmt.Fprintf(os.Stderr, "unhandled node: %T\n", n)
	}
	return
}

func (s *server) DidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) (err error) {
	defer s.publishDiagnostics(params.TextDocument.URI)
	doc := document{item: params.TextDocument}
	doc.ast, doc.err = jsonnet.SnippetToAST(params.TextDocument.URI.SpanURI().Filename(), params.TextDocument.Text)
	if doc.err != nil {
		return s.cache.put(doc)
	}
	symbols := analyseSymbols(doc.ast)
	if len(symbols) != 1 {
		panic("There should only be a single root symbol for an AST")
	}
	doc.symbols = symbols[0]
	// TODO: Work out better way to invalidate the VM cache.
	doc.val, doc.err = s.vm.EvaluateAnonymousSnippet(params.TextDocument.URI.SpanURI().Filename(), params.TextDocument.Text)
	return s.cache.put(doc)
}

func (s *server) DidRenameFiles(context.Context, *protocol.RenameFilesParams) error {
	return notImplemented("DidRenameFiles")
}

func (s *server) DidSave(context.Context, *protocol.DidSaveTextDocumentParams) error {
	return notImplemented("DidSave")
}

func (s *server) DocumentColor(context.Context, *protocol.DocumentColorParams) ([]protocol.ColorInformation, error) {
	return nil, notImplemented("DocumentColor")
}

func (s *server) DocumentHighlight(context.Context, *protocol.DocumentHighlightParams) ([]protocol.DocumentHighlight, error) {
	return nil, notImplemented("DocumentHighlight")
}

func (s *server) DocumentLink(context.Context, *protocol.DocumentLinkParams) ([]protocol.DocumentLink, error) {
	// TODO: This is not implemented but I was unable to disable the capability.
	return nil, nil
}

func (s *server) DocumentSymbol(ctx context.Context, params *protocol.DocumentSymbolParams) ([]interface{}, error) {
	doc, err := s.cache.get(params.TextDocument.URI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "DocumentSymbol: unable to get document from cache: %v\n", err)
		return nil, err
	}

	return []interface{}{doc.symbols}, nil
}

func (s *server) ExecuteCommand(context.Context, *protocol.ExecuteCommandParams) (interface{}, error) {
	return nil, notImplemented("ExecuteCommand")
}

func (s *server) Exit(context.Context) error {
	return notImplemented("Exit")
}

func (s *server) FoldingRange(context.Context, *protocol.FoldingRangeParams) ([]protocol.FoldingRange, error) {
	return nil, notImplemented("FoldingRange")
}

func (s *server) Formatting(ctx context.Context, params *protocol.DocumentFormattingParams) ([]protocol.TextEdit, error) {
	doc, err := s.cache.get(params.TextDocument.URI)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve document from cache: %w", err)
	}
	// TODO: This should be user configurable.
	formatted, err := formatter.Format(params.TextDocument.URI.SpanURI().Filename(), doc.item.Text, formatter.DefaultOptions())
	if err != nil {
		return nil, fmt.Errorf("unable to format document: %w", err)
	}
	// TODO: Consider applying individual edits.
	return []protocol.TextEdit{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 0},
				End:   protocol.Position{Line: 0, Character: 0},
			},
			NewText: formatted,
		},
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 0},
				End:   protocol.Position{Line: uint32(strings.Count(formatted+doc.item.Text, "\n")), Character: ^uint32(0)},
			},
			NewText: "",
		},
	}, nil
}

func (s *server) Hover(context.Context, *protocol.HoverParams) (*protocol.Hover, error) {
	return nil, notImplemented("Hover")
}

func (s *server) Implementation(context.Context, *protocol.ImplementationParams) (protocol.Definition, error) {
	return nil, notImplemented("Implementation")
}

func (s *server) IncomingCalls(context.Context, *protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	return nil, notImplemented("IncomingCalls")
}

func (s *server) Initialize(ctx context.Context, params *protocol.ParamInitialize) (*protocol.InitializeResult, error) {
	return &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			DefinitionProvider:         true,
			DocumentSymbolProvider:     true,
			DocumentFormattingProvider: true,
			TextDocumentSync: &protocol.TextDocumentSyncOptions{
				Change:    protocol.Full,
				OpenClose: true,
				Save: protocol.SaveOptions{
					IncludeText: false,
				},
			},
		},
		ServerInfo: struct {
			Name    string `json:"name"`
			Version string `json:"version,omitempty"`
		}{
			Name: "jsonnet-language-server",
		},
	}, nil
}

func (s *server) Initialized(context.Context, *protocol.InitializedParams) error {
	return nil
}

func (s *server) LinkedEditingRange(context.Context, *protocol.LinkedEditingRangeParams) (*protocol.LinkedEditingRanges, error) {
	return nil, notImplemented("LinkedEditingRange")
}

func (s *server) LogTrace(context.Context, *protocol.LogTraceParams) error {
	return notImplemented("LogTrace")
}

func (s *server) Moniker(context.Context, *protocol.MonikerParams) ([]protocol.Moniker, error) {
	return nil, notImplemented("Moniker")
}

func (s *server) NonstandardRequest(context.Context, string, interface{}) (interface{}, error) {
	return nil, notImplemented("NonstandardRequest")
}

func (s *server) OnTypeFormatting(context.Context, *protocol.DocumentOnTypeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, notImplemented("OnTypeFormatting")
}

func (s *server) OutgoingCalls(context.Context, *protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	return nil, notImplemented("OutgoingCalls")
}

func (s *server) PrepareCallHierarchy(context.Context, *protocol.CallHierarchyPrepareParams) ([]protocol.CallHierarchyItem, error) {
	return nil, notImplemented("PrepareCallHierarchy")
}

func (s *server) PrepareRename(context.Context, *protocol.PrepareRenameParams) (*protocol.Range, error) {
	return nil, notImplemented("PrepareRange")
}

func (s *server) PrepareTypeHierarchy(context.Context, *protocol.TypeHierarchyPrepareParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, notImplemented("PrepareTypeHierarchy")
}

func (s *server) RangeFormatting(context.Context, *protocol.DocumentRangeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, notImplemented("RangeFormatting")
}

func (s *server) References(context.Context, *protocol.ReferenceParams) ([]protocol.Location, error) {
	return nil, notImplemented("References")
}

func (s *server) Rename(context.Context, *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	return nil, notImplemented("Rename")
}

func (s *server) Resolve(context.Context, *protocol.CompletionItem) (*protocol.CompletionItem, error) {
	return nil, notImplemented("Resolve")
}

func (s *server) ResolveCodeAction(context.Context, *protocol.CodeAction) (*protocol.CodeAction, error) {
	return nil, notImplemented("ResolveCodeAction")
}

func (s *server) ResolveCodeLens(context.Context, *protocol.CodeLens) (*protocol.CodeLens, error) {
	return nil, notImplemented("ResolveCodeLens")
}

func (s *server) ResolveDocumentLink(context.Context, *protocol.DocumentLink) (*protocol.DocumentLink, error) {
	return nil, notImplemented("ResolveDocumentLink")
}

func (s *server) SelectionRange(context.Context, *protocol.SelectionRangeParams) ([]protocol.SelectionRange, error) {
	return nil, notImplemented("SelectionRange")
}

func (s *server) SemanticTokensFull(context.Context, *protocol.SemanticTokensParams) (*protocol.SemanticTokens, error) {
	return nil, notImplemented("SemanticTokensFull")
}

func (s *server) SemanticTokensFullDelta(context.Context, *protocol.SemanticTokensDeltaParams) (interface{}, error) {
	return nil, notImplemented("SemanticTokensFullDelta")
}

func (s *server) SemanticTokensRange(context.Context, *protocol.SemanticTokensRangeParams) (*protocol.SemanticTokens, error) {
	return nil, notImplemented("SemanticTokensRange")
}

func (s *server) SemanticTokensRefresh(context.Context) error {
	return notImplemented("SemanticTokensRefresh")
}

func (s *server) SetTrace(context.Context, *protocol.SetTraceParams) error {
	return notImplemented("SetTrace")
}

func (s *server) Shutdown(context.Context) error {
	return notImplemented("Shutdown")
}

func (s *server) SignatureHelp(context.Context, *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	return nil, notImplemented("SignatureHelp")
}

func (s *server) Subtypes(context.Context, *protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, notImplemented("Subtypes")
}

func (s *server) Supertypes(context.Context, *protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, notImplemented("Supertypes")
}

func (s *server) Symbol(context.Context, *protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return nil, notImplemented("Symbol")
}

func (s *server) TypeDefinition(context.Context, *protocol.TypeDefinitionParams) (protocol.Definition, error) {
	return nil, notImplemented("TypeDefinition")
}

func (s *server) WillCreateFiles(context.Context, *protocol.CreateFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, notImplemented("WillCreateFiles")
}

func (s *server) WillDeleteFiles(context.Context, *protocol.DeleteFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, notImplemented("WillDeleteFiles")
}

func (s *server) WillRenameFiles(context.Context, *protocol.RenameFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, notImplemented("WillRenameFiles")
}

func (s *server) WillSave(context.Context, *protocol.WillSaveTextDocumentParams) error {
	return notImplemented("WillSave")
}

func (s *server) WillSaveWaitUntil(context.Context, *protocol.WillSaveTextDocumentParams) ([]protocol.TextEdit, error) {
	return nil, notImplemented("WillSaveWaitUntil")
}

func (s *server) WorkDoneProgressCancel(context.Context, *protocol.WorkDoneProgressCancelParams) error {
	return notImplemented("WorkDoneProgressCancel")
}

func notImplemented(method string) error {
	return fmt.Errorf("%w: %q not yet implemented", jsonrpc2.ErrMethodNotFound, method)
}
