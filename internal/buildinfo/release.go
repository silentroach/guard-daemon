// Package buildinfo содержит идентификатор воспроизводимой релизной сборки.
package buildinfo

// ReleaseCommit задаётся только при сборке релиза через компоновщик Go. Пустое
// значение означает обычную отладочную сборку из зафиксированной версии исходного кода.
var ReleaseCommit string

// Version возвращает только безопасный публичный идентификатор бинарного файла.
func Version() string {
	if len(ReleaseCommit) != 40 {
		return "development"
	}
	for _, character := range ReleaseCommit {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "development"
		}
	}
	return ReleaseCommit
}
