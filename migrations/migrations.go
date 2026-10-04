// Package migrations embute os arquivos SQL versionados no binário.
//
// Formato golang-migrate: NNNNNN_nome.up.sql / NNNNNN_nome.down.sql.
package migrations

import "embed"

// FS contém todos os arquivos .sql deste diretório.
//
//go:embed *.sql
var FS embed.FS
