package wasmhost

import (
	"bytes"
	"encoding/binary"
	"io"

	"papo/internal/plugins"
)

// preflight recusa start automático e imports não funcionais antes da compilação.
// A validação estrutural completa permanece responsabilidade do wazero.
func preflight(data []byte) error {
	if len(data) < 8 || !bytes.Equal(data[:8], []byte{0, 97, 115, 109, 1, 0, 0, 0}) {
		return plugins.ErrInvalidArtifact
	}
	r := bytes.NewReader(data[8:])
	for r.Len() > 0 {
		section, err := r.ReadByte()
		if err != nil {
			return plugins.ErrInvalidArtifact
		}
		n, err := binary.ReadUvarint(r)
		if err != nil || n > uint64(r.Len()) {
			return plugins.ErrInvalidArtifact
		}
		payload := make([]byte, int(n))
		if _, err = io.ReadFull(r, payload); err != nil {
			return plugins.ErrInvalidArtifact
		}
		if section == 8 {
			return plugins.ErrInvalidArtifact
		}

		// Limita tabelas de funções: WithMemoryLimitPages cobre apenas memória linear.
		if section == 4 {
			table := bytes.NewReader(payload)
			count, err := binary.ReadUvarint(table)
			if err != nil || count > 1 {
				return plugins.ErrInvalidArtifact
			}
			for i := uint64(0); i < count; i++ {
				ref, err := table.ReadByte()
				if err != nil || ref != 0x70 {
					return plugins.ErrInvalidArtifact
				}
				flags, err := table.ReadByte()
				if err != nil || flags != 1 {
					return plugins.ErrInvalidArtifact
				}
				min, err := binary.ReadUvarint(table)
				if err != nil {
					return plugins.ErrInvalidArtifact
				}
				max, err := binary.ReadUvarint(table)
				if err != nil || min > max || max > 4096 {
					return plugins.ErrInvalidArtifact
				}
			}
			if table.Len() != 0 {
				return plugins.ErrInvalidArtifact
			}
		}
		// Contagens também limitam metadados/compilação de módulos pequenos artificiais.
		if section == 1 || section == 3 || section == 6 || section == 9 || section == 11 {
			count, err := binary.ReadUvarint(bytes.NewReader(payload))
			if err != nil || count > 4096 {
				return plugins.ErrInvalidArtifact
			}
		}
		if section != 2 {
			continue
		}
		imports := bytes.NewReader(payload)
		count, err := binary.ReadUvarint(imports)
		if err != nil || count > 64 {
			return plugins.ErrInvalidArtifact
		}
		for i := uint64(0); i < count; i++ {
			for j := 0; j < 2; j++ {
				size, err := binary.ReadUvarint(imports)
				if err != nil || size > uint64(imports.Len()) {
					return plugins.ErrInvalidArtifact
				}
				imports.Seek(int64(size), io.SeekCurrent)
			}
			kind, err := imports.ReadByte()
			if err != nil || kind != 0 {
				return plugins.ErrInvalidArtifact
			}
			if _, err = binary.ReadUvarint(imports); err != nil {
				return plugins.ErrInvalidArtifact
			}
		}
		if imports.Len() != 0 {
			return plugins.ErrInvalidArtifact
		}
	}
	return nil
}
