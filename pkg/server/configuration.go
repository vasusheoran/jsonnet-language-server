package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/formatter"
	"github.com/jdbaldry/go-language-server-protocol/jsonrpc2"
	"github.com/jdbaldry/go-language-server-protocol/lsp/protocol"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
)

type Configuration struct {
	ResolvePathsWithTanka bool
	JPaths                []string
	TLACode               map[string]string
	ExtVars               map[string]string
	ExtCode               map[string]string
	FormattingOptions     formatter.Options

	EnableEvalDiagnostics     bool
	EnableLintDiagnostics     bool
	ShowDocstringInCompletion bool
}

func (s *Server) DidChangeConfiguration(_ context.Context, params *protocol.DidChangeConfigurationParams) error {
	settingsMap, ok := params.Settings.(map[string]interface{})
	if !ok {
		return fmt.Errorf("%w: unsupported settings payload. expected json object, got: %T", jsonrpc2.ErrInvalidParams, params.Settings)
	}

	for sk, sv := range settingsMap {
		switch sk {
		case "log_level":
			level, err := log.ParseLevel(sv.(string))
			if err != nil {
				return fmt.Errorf("%w: %v", jsonrpc2.ErrInvalidParams, err)
			}
			log.SetLevel(level)
		case "resolve_paths_with_tanka":
			if boolVal, ok := sv.(bool); ok {
				s.configuration.ResolvePathsWithTanka = boolVal
			} else {
				return fmt.Errorf("%w: unsupported settings value for resolve_paths_with_tanka. expected boolean. got: %T", jsonrpc2.ErrInvalidParams, sv)
			}
		case "jpath":
			if svList, ok := sv.([]interface{}); ok {
				s.configuration.JPaths = make([]string, len(svList))
				for i, v := range svList {
					if strVal, ok := v.(string); ok {
						s.configuration.JPaths[i] = strVal
					} else {
						return fmt.Errorf("%w: unsupported settings value for jpath. expected string. got: %T", jsonrpc2.ErrInvalidParams, v)
					}
				}
			} else {
				return fmt.Errorf("%w: unsupported settings value for jpath. expected array of strings. got: %T", jsonrpc2.ErrInvalidParams, sv)
			}

		case "enable_eval_diagnostics":
			if boolVal, ok := sv.(bool); ok {
				s.configuration.EnableEvalDiagnostics = boolVal
			} else {
				return fmt.Errorf("%w: unsupported settings value for enable_eval_diagnostics. expected boolean. got: %T", jsonrpc2.ErrInvalidParams, sv)
			}
		case "enable_lint_diagnostics":
			if boolVal, ok := sv.(bool); ok {
				s.configuration.EnableLintDiagnostics = boolVal
			} else {
				return fmt.Errorf("%w: unsupported settings value for enable_lint_diagnostics. expected boolean. got: %T", jsonrpc2.ErrInvalidParams, sv)
			}
		case "show_docstring_in_completion":
			if boolVal, ok := sv.(bool); ok {
				s.configuration.ShowDocstringInCompletion = boolVal
			} else {
				return fmt.Errorf("%w: unsupported settings value for show_docstring_in_completion. expected boolean. got: %T", jsonrpc2.ErrInvalidParams, sv)
			}
		case "ext_vars":
			newVars, err := s.parseExtVars(sv)
			if err != nil {
				return fmt.Errorf("%w: ext_vars parsing failed: %v", jsonrpc2.ErrInvalidParams, err)
			}
			s.configuration.ExtVars = newVars
		case "formatting":
			newFmtOpts, err := s.parseFormattingOpts(sv)
			if err != nil {
				return fmt.Errorf("%w: formatting options parsing failed: %v", jsonrpc2.ErrInvalidParams, err)
			}
			s.configuration.FormattingOptions = newFmtOpts

		case "ext_code":
			newCode, err := s.parseExtCode(sv)
			if err != nil {
				return fmt.Errorf("%w: ext_code parsing failed: %v", jsonrpc2.ErrInvalidParams, err)
			}
			s.configuration.ExtCode = newCode
		case "tla_code":
			newTLACode, err := s.parseTLACode(sv)
			if err != nil {
				return fmt.Errorf("%w: tla_code parsing failed: %v", jsonrpc2.ErrInvalidParams, err)
			}
			s.configuration.TLACode = newTLACode

		default:
			return fmt.Errorf("%w: unsupported settings key: %q", jsonrpc2.ErrInvalidParams, sk)
		}
	}
	log.Infof("configuration updated: %+v", s.configuration)

	return nil
}

func (s *Server) parseExtVars(unparsed interface{}) (map[string]string, error) {
	newVars, ok := unparsed.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unsupported settings value for ext_vars. expected json object. got: %T", unparsed)
	}

	extVars := make(map[string]string, len(newVars))
	for varKey, varValue := range newVars {
		vv, ok := varValue.(string)
		if !ok {
			return nil, fmt.Errorf("unsupported settings value for ext_vars.%s. expected string. got: %T", varKey, varValue)
		}
		extVars[varKey] = vv
	}
	return extVars, nil
}

func (s *Server) parseFormattingOpts(unparsed interface{}) (formatter.Options, error) {
	newOpts, ok := unparsed.(map[string]interface{})
	if !ok {
		return formatter.Options{}, fmt.Errorf("unsupported settings value for formatting. expected json object. got: %T", unparsed)
	}

	opts := formatter.DefaultOptions()
	config := mapstructure.DecoderConfig{
		Result: &opts,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			stringStyleDecodeFunc,
			commentStyleDecodeFunc,
		),
	}
	decoder, err := mapstructure.NewDecoder(&config)
	if err != nil {
		return formatter.Options{}, fmt.Errorf("decoder construction failed: %v", err)
	}

	if err := decoder.Decode(newOpts); err != nil {
		return formatter.Options{}, fmt.Errorf("map decode failed: %v", err)
	}
	return opts, nil
}

func (s *Server) parseExtCode(unparsed interface{}) (map[string]string, error) {
	newVars, ok := unparsed.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unsupported settings value for ext_code. expected json object. got: %T", unparsed)
	}

	vm := s.getVM(".")

	extCode := make(map[string]string, len(newVars))
	for varKey, varValue := range newVars {
		vv, ok := varValue.(interface{})
		if !ok {
			return nil, fmt.Errorf("unsupported settings value for ext_code.%s. expected string. got: %T", varKey, varValue)
		}

		valueBytes, err := json.Marshal(vv)
		if err != nil {
			msg := fmt.Sprintf("error marshaling ext_code.%s, err: %s", varKey, err.Error())
			return nil, fmt.Errorf(msg)
		}

		jsonResult, err := vm.EvaluateAnonymousSnippet("ext-code", string(valueBytes))
		if err != nil {
			msg := fmt.Sprintf("error EvaluateAnonymousSnippet ext_code.%s, err: %s", varKey, err.Error())
			return nil, fmt.Errorf(msg)

		}
		extCode[varKey] = jsonResult
	}

	return extCode, nil
}

func (s *Server) parseTLACode(unparsed interface{}) (map[string]string, error) {
	log.WithFields(log.Fields{"method": "parseTLACode"})
	newTLACode, ok := unparsed.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unsupported settings value for tla_code. expected json object. got: %T", unparsed)
	}

	vm := s.getVM(".")
	tlaCode := make(map[string]string, len(newTLACode))
	for varKey, varValue := range newTLACode {
		vv, ok := varValue.(interface{})
		if !ok {
			log.Debugf("error: [%s]", fmt.Errorf("unsupported settings value for tla_code.%s. expected interface{}. got: %T", varKey, varValue))
			return nil, fmt.Errorf("unsupported settings value for tla_code.%s. expected interface{}. got: %T", varKey, varValue)
		}
		valueBytes, err := json.Marshal(vv)
		if err != nil {
			msg := fmt.Sprintf("error marshaling tla_code.%s, err: %s", varKey, err.Error())
			return nil, fmt.Errorf(msg)
		}

		jsonResult, err := vm.EvaluateAnonymousSnippet("tla-code", string(valueBytes))
		if err != nil {
			msg := fmt.Sprintf("error evaluating EvaluateAnonymousSnippet tla_code.%s, err: %s", varKey, err.Error())
			return nil, fmt.Errorf(msg)

		}
		tlaCode[varKey] = jsonResult
	}
	log.Debugf("tlaCode: [%#v]", newTLACode)

	return tlaCode, nil
}

func resetExtVars(vm *jsonnet.VM, vars map[string]string, code map[string]string) {
	for vk, vv := range vars {
		vm.ExtVar(vk, vv)
	}
	for vk, vv := range code {
		vm.ExtCode(vk, vv)
	}
}

func resetTLACode(vm *jsonnet.VM, tlaCode map[string]string) {
	for vk, vv := range tlaCode {
		vm.TLACode(vk, vv)
	}
}

func stringStyleDecodeFunc(_, to reflect.Type, unparsed interface{}) (interface{}, error) {
	if to != reflect.TypeOf(formatter.StringStyleDouble) {
		return unparsed, nil
	}

	str, ok := unparsed.(string)
	if !ok {
		return nil, fmt.Errorf("expected string, got: %T", unparsed)
	}
	// will not panic because of the kind == string check above
	switch str {
	case "double":
		return formatter.StringStyleDouble, nil
	case "single":
		return formatter.StringStyleSingle, nil
	case "leave":
		return formatter.StringStyleLeave, nil
	default:
		return nil, fmt.Errorf("expected one of 'double', 'single', 'leave', got: %q", str)
	}
}

func commentStyleDecodeFunc(_, to reflect.Type, unparsed interface{}) (interface{}, error) {
	if to != reflect.TypeOf(formatter.CommentStyleHash) {
		return unparsed, nil
	}

	str, ok := unparsed.(string)
	if !ok {
		return nil, fmt.Errorf("expected string, got: %T", unparsed)
	}
	switch str {
	case "hash":
		return formatter.CommentStyleHash, nil
	case "slash":
		return formatter.CommentStyleSlash, nil
	case "leave":
		return formatter.CommentStyleLeave, nil
	default:
		return nil, fmt.Errorf("expected one of 'hash', 'slash', 'leave', got: %q", str)
	}
}
