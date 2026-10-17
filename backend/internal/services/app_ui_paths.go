package services

import "strings"

// pluginAssetURL devolve o caminho público de um arquivo de ui/ da versão
// aprovada: /apps/<instalação>/ui/<digest>/<arquivo>. Como o digest muda a
// cada versão, a URL é imutável e pode ser cacheada indefinidamente; caminhos
// relativos dentro das páginas continuam presos à mesma versão.
func pluginAssetURL(installationID, digest, file string) string {
	return "/apps/" + installationID + "/ui/" + digest + "/" + strings.TrimPrefix(file, "ui/")
}
