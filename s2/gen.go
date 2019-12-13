//+build ignore

//go:generate go run gen.go -out encodeblock_amd64.s -stubs encodeblock_amd64.go

package main

import (
	"fmt"

	. "github.com/mmcloughlin/avo/build"
	"github.com/mmcloughlin/avo/buildtags"
	. "github.com/mmcloughlin/avo/operand"
	"github.com/mmcloughlin/avo/reg"
)

func main() {
	Constraint(buildtags.Not("appengine").ToConstraint())
	Constraint(buildtags.Not("noasm").ToConstraint())
	Constraint(buildtags.Term("gc").ToConstraint())

	genEncodeBlockAsm()
	genEmitLiteral()
	Generate()
}

func genEncodeBlockAsm() {
	TEXT("genEncodeBlockAsm", NOSPLIT, "func(dst, src []byte) int")
	Doc("encodeBlock encodes a non-empty src to a guaranteed-large-enough dst.",
		"It assumes that the varint-encoded length of the decompressed bytes has already been written.", "")
	Pragma("noescape")

	// "var table [maxTableSize]uint32" takes up 65536 bytes of stack space. An
	// extra 56 bytes, to call other functions, and an extra 64 bytes, to spill
	// local variables (registers) during calls gives 65536 + 56 + 64 = 65656.
	const (
		tableBits  = 16
		tableSize  = 1 << 16
		tableMask  = tableSize - 1
		baseStack  = 56
		extraStack = 64
		allocStack = baseStack + extraStack + tableSize
	)

	stack := AllocLocal(allocStack)
	table := stack.Offset(allocStack - tableSize)

	tmpStack := baseStack
	sLimit := stack.Offset(tmpStack)
	tmpStack += 4
	dstLimit := stack.Offset(tmpStack)
	tmpStack += 4
	nextEmit := stack.Offset(tmpStack)
	tmpStack += 4
	repeat := stack.Offset(tmpStack)
	tmpStack += 4

	if tmpStack > extraStack+baseStack {
		panic(fmt.Sprintf("tmp stack exceeded", tmpStack))
	}
	// Zero table
	iReg := GP64()
	MOVQ(U32(tableSize/8/16), iReg)
	tablePtr := GP64()
	LEAQ(table, tablePtr)
	XORQ(iReg, iReg)
	zeroXmm := XMM()
	PXOR(zeroXmm, zeroXmm)

	Label("zeroloop")
	for i := 0; i < 8; i++ {
		MOVUPS(zeroXmm, Mem{Base: tablePtr}.Offset(i*16))
	}
	ADDQ(Imm(16*8), tablePtr)
	DECQ(iReg)
	JNZ(LabelRef("zeroloop"))

	hasher := hash6(tableBits)

	//src := Load(Param("src"), GP64())
	s := GP64()
	XORQ(s, s)
	//dst := Load(Param("dst"), GP64())

	_, _, _, _, _ = sLimit, dstLimit, nextEmit, repeat, hasher

	RET()
}

type hashGen struct {
	bytes     int
	tablebits int
	mulreg    reg.GPVirtual
}

// hash uses multiply to get a 'output' hash on the hash of the lowest 'bytes' bytes in value.
func hash6(tablebits int) hashGen {
	h := hashGen{
		bytes:     6,
		tablebits: tablebits,
		mulreg:    GP64(),
	}
	MOVQ(Imm(227718039650203), h.mulreg)
	return h
}

// hash uses multiply to get hash of the value.
func (h hashGen) hash(val reg.GPVirtual) {
	// Move value to top of register.
	SHLQ(U8(64-8*h.bytes), val)
	IMULQ(h.mulreg, val)
	// Move value to bottom
	SHRQ(U8(64-h.tablebits), val)
}

func genEmitLiteral() {
	TEXT("emitLiteral", NOSPLIT, "func(dst, lit []byte) int")
	Doc("encodeBlock encodes a non-empty src to a guaranteed-large-enough dst.",
		"It assumes that the varint-encoded length of the decompressed bytes has already been written.", "")
	Pragma("noescape")

	// The 24 bytes of stack space is to call runtime·memmove.
	AllocLocal(24)
	retval := GP64().As64()

	dstBase := Load(Param("dst").Base(), GP64())
	litBase := Load(Param("lit").Base(), GP64())
	litLen := Load(Param("lit").Len(), GP64())
	emitLiteral("Standalone", GP64(), GP64(), litLen, retval, dstBase, litBase)
	Store(retval, ReturnIndex(0))
	RET()
}

func emitLiteral(name string, tmp1, tmp2 reg.GPVirtual, litLen, retval, dstBase, litBase reg.Register) {
	n := tmp1
	n16 := tmp2

	// We always add litLen bytes
	MOVQ(litLen, retval)
	MOVQ(litLen, n)
	MOVQ(litLen, n16)

	SUBL(U8(1), n.As32())
	// Return if AX was 0
	JC(LabelRef("emitLiteralEnd" + name))

	// Find number of bytes to emit for tag.
	CMPL(n.As32(), U8(60))
	JLT(LabelRef("oneByte" + name))
	CMPL(n.As32(), U32(256))
	JLT(LabelRef("twoBytes" + name))
	CMPL(n.As32(), U32(65536))
	JLT(LabelRef("threeBytes" + name))
	CMPL(n.As32(), U32(16777216))
	JLT(LabelRef("fourBytes" + name))

	Label("fiveBytes" + name)
	MOVB(U8(252), Mem{Base: dstBase})
	MOVL(n.As32(), Mem{Base: dstBase, Disp: 1})
	ADDQ(U8(5), retval)
	ADDQ(U8(5), dstBase)
	JMP(LabelRef("memmove" + name))

	Label("fourBytes" + name)
	NOP()
	Label("threeBytes" + name)
	NOP()
	Label("twoBytes" + name)
	NOP()
	Label("oneByte" + name)
	NOP()
	Label("memmove" + name)
	NOP()

	Label("emitLiteralEnd" + name)
	NOP()
	/*

		// func emitLiteral(dst, lit []byte) int
		//
		// All local variables fit into registers. The register allocation:
		//	- AX	len(lit)
		//	- BX	n
		//  - CX    n >> 16
		//	- DX	return value
		//	- DI	&dst[i]
		//	- R10	&lit[0]
		//
		//
		TEXT ·emitLiteral(SB), NOSPLIT, $24-56
			MOVQ dst_base+0(FP), DI
			MOVQ lit_base+24(FP), R10
			MOVQ lit_len+32(FP), AX
			MOVQ AX, DX
			MOVL AX, BX

			SUBL $1, BX

			// Return if AX was 0
			JC zero_end

			MOVQ BX, CX
			CMPL BX, $60
			JLT  oneByte
			CMPL BX, $256
			JLT  twoBytes
			CMPL BX, $65536
			JLT  threeBytes
			CMPL BX, $16777216
			JLT  fourBytes

		fiveBytes:
			MOVB $252, 0(DI)
			MOVL BX, 1(DI)
			ADDQ $5, DI
			ADDQ $5, DX
			JMP  memmove

		fourBytes:
			SHRL $16, CX
			MOVB $248, 0(DI)
			MOVW BX, 1(DI)
			MOVB CX, 3(DI)
			ADDQ $4, DI
			ADDQ $4, DX
			JMP  memmove

		threeBytes:
			MOVB $0xf4, 0(DI)
			MOVW BX, 1(DI)
			ADDQ $3, DI
			ADDQ $3, DX
			JMP  memmove

		twoBytes:
			MOVB $0xf0, 0(DI)
			MOVB BX, 1(DI)
			ADDQ $2, DI
			ADDQ $2, DX
			JMP  memmove

		oneByte:
			SHLB $2, BX
			MOVB BX, 0(DI)
			ADDQ $1, DI
			ADDQ $1, DX

		memmove:
			// Store return value
			MOVQ DX, ret+48(FP)

			// copy(dst[i:], lit)
			//
			// This means calling runtime·memmove(&dst[i], &lit[0], len(lit)), so we push
			// DI, R10 and AX as arguments.
			MOVQ DI, 0(SP)
			MOVQ R10, 8(SP)
			MOVQ AX, 16(SP)
			CALL runtime·memmove(SB)
			RET

		zero_end:
			MOVQ $0, ret+48(FP)
			RET

	*/
}
