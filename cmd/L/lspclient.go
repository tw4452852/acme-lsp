package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"9fans.net/go/plan9"
	"9fans.net/go/plan9/client"
	"9fans.net/go/plumb"
	"github.com/pkg/errors"
	lsp "github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
)

type lspHandler struct {
	mu sync.Mutex
}

func (h *lspHandler) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	if strings.HasPrefix(req.Method, "$/") {
		// Ignore server dependent notifications
		if *debug {
			fmt.Printf("Handle: got request %#v\n", req)
		}
		return
	}
	switch req.Method {
	case "textDocument/publishDiagnostics":
		var params lsp.PublishDiagnosticsParams
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			log.Printf("diagnostics unmarshal failed: %v\n", err)
			return
		}
		for _, diag := range params.Diagnostics {
			fmt.Printf("Diagnostic: %v: %#v\n", params.URI, diag)
		}
	default:
		fmt.Printf("Handle: got request %#v\n", req)
	}
}

type lspClient struct {
	rpc *jsonrpc2.Conn
	ctx context.Context

	plumber *client.Fid
}

func newLSPClient(conn net.Conn) (*lspClient, error) {
	ctx := context.Background()
	stream := jsonrpc2.NewBufferedStream(conn, jsonrpc2.VSCodeObjectCodec{})
	rpc := jsonrpc2.NewConn(ctx, stream, &lspHandler{})

	initp := &lsp.InitializeParams{
		RootURI: "file:///",
	}
	initr := &lsp.InitializeResult{}
	if err := rpc.Call(ctx, "initialize", initp, initr); err != nil {
		return nil, errors.Wrap(err, "initialize failed")
	}
	p, err := plumb.Open("send", plan9.OWRITE)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open plumber")
	}
	return &lspClient{
		rpc:     rpc,
		ctx:     ctx,
		plumber: p,
	}, nil
}

func (c *lspClient) Close() error {
	c.plumber.Close()
	return c.rpc.Close()
}

func (c *lspClient) Plumb(data []byte) error {
	m := &plumb.Message{
		Src:  "L",
		Dst:  "edit",
		Dir:  "/",
		Type: "text",
		Data: data,
	}
	return m.Send(c.plumber)
}

func (c *lspClient) PlumbLocation(loc *lsp.Location) error {
	fn := uriToFilename(string(loc.URI))
	a := fmt.Sprintf("%v:%v", fn, loc.Range.Start)
	return c.Plumb([]byte(a))
}

func locToLink(l *lsp.Location) string {
	p := uriToFilename(string(l.URI))
	return fmt.Sprintf("%s:%v:%v-%v:%v", p,
		l.Range.Start.Line+1, l.Range.Start.Character+1,
		l.Range.End.Line+1, l.Range.End.Character+1)
}

func (c *lspClient) Definition(pos *lsp.TextDocumentPositionParams) error {
	loc := make([]lsp.Location, 1)
	if err := c.rpc.Call(c.ctx, "textDocument/definition", pos, &loc); err != nil {
		return err
	}
	for _, l := range loc {
		c.PlumbLocation(&l)
	}
	return nil
}

func (c *lspClient) Hover(pos *lsp.TextDocumentPositionParams, w io.Writer) error {
	var hov Hover
	if err := c.rpc.Call(c.ctx, "textDocument/hover", pos, &hov); err != nil {
		return err
	}
	for _, c := range hov.Contents {
		fmt.Fprintf(w, "%v\n", c.Value)
	}
	return nil
}

func (c *lspClient) References(pos *lsp.TextDocumentPositionParams, w io.Writer) error {
	rp := &lsp.ReferenceParams{
		TextDocumentPositionParams: *pos,
		Context: lsp.ReferenceContext{
			IncludeDeclaration: true,
		},
	}
	loc := make([]lsp.Location, 1)
	if err := c.rpc.Call(c.ctx, "textDocument/references", rp, &loc); err != nil {
		return err
	}
	for _, l := range loc {
		fmt.Fprintf(w, " %v\n", locToLink(&l))
	}
	return nil
}

func (c *lspClient) Completion(pos *lsp.TextDocumentPositionParams, w io.Writer) error {
	comp := &lsp.CompletionParams{
		TextDocumentPositionParams: *pos,
		Context:                    lsp.CompletionContext{},
	}
	var cl lsp.CompletionList
	if err := c.rpc.Call(c.ctx, "textDocument/completion", comp, &cl); err != nil {
		return err
	}
	if len(cl.Items) == 0 {
		fmt.Fprintf(w, "no completion\n")
	}
	for _, item := range cl.Items {
		fmt.Fprintf(w, "%v %v\n", item.Label, item.Detail)
	}
	return nil
}

func (c *lspClient) SignatureHelp(pos *lsp.TextDocumentPositionParams, w io.Writer) error {
	var sh lsp.SignatureHelp
	if err := c.rpc.Call(c.ctx, "textDocument/signatureHelp", pos, &sh); err != nil {
		return err
	}
	for _, sig := range sh.Signatures {
		fmt.Fprintf(w, "%v\n", sig.Label)
		fmt.Fprintf(w, "%v\n", sig.Documentation)
	}
	return nil
}

func (c *lspClient) Rename(pos *lsp.TextDocumentPositionParams, newname string) error {
	params := &lsp.RenameParams{
		TextDocument: pos.TextDocument,
		Position:     pos.Position,
		NewName:      newname,
	}
	var we lsp.WorkspaceEdit
	if err := c.rpc.Call(c.ctx, "textDocument/rename", params, &we); err != nil {
		return err
	}
	return applyAcmeEdits(&we)
}

func (c *lspClient) Format(pos *lsp.TextDocumentPositionParams) error {
	params := &lsp.DocumentFormattingParams{
		TextDocument: pos.TextDocument,
	}
	var edits []lsp.TextEdit
	if err := c.rpc.Call(c.ctx, "textDocument/formatting", params, &edits); err != nil {
		return err
	}
	id, err := strconv.Atoi(os.Getenv("winid"))
	if err != nil {
		return errors.Wrapf(err, "invalid $winid")
	}
	if err := applyWinEdits(id, edits); err != nil {
		return errors.Wrapf(err, "failed to apply edits to window %v", id)
	}
	return nil
}

func (c *lspClient) DidOpen(filename string, body []byte) error {
	lang := filepath.Ext(filename)
	switch lang {
	case "py":
		lang = "python"
	}
	params := &lsp.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{
			URI:        lsp.DocumentURI("file://" + filename),
			LanguageID: lang,
			Version:    0,
			Text:       string(body),
		},
	}
	return c.rpc.Notify(c.ctx, "textDocument/didOpen", params)
}

func (c *lspClient) DidClose(filename string) error {
	params := &lsp.DidCloseTextDocumentParams{
		TextDocument: lsp.TextDocumentIdentifier{
			URI: lsp.DocumentURI("file://" + filename),
		},
	}
	return c.rpc.Notify(c.ctx, "textDocument/didClose", params)
}
