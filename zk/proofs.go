// Copyright (c) 2024 The illium developers
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.

package zk

/*
#include <stdlib.h>
#include <stdint.h>
#include <unistd.h>
#include <fcntl.h>
#include <stdbool.h>
// Function to save the current stderr file descriptor and redirect stderr to /dev/null
int redirect_stderr() {
    int stderr_copy = dup(2);
    int dev_null = open("/dev/null", O_WRONLY);
    dup2(dev_null, 2);
    close(dev_null);
    return stderr_copy;
}
// Function to restore stderr from the saved file descriptor
void restore_stderr(int stderr_copy) {
    dup2(stderr_copy, 2);
    close(stderr_copy);
}
void load_public_params();
int create_proof_ffi(
    const char* lurk_program,
    const char* private_params,
    const char* public_params,
 	size_t* max_steps,
    uint8_t* proof,
    size_t* proof_len,
    uint8_t* output_tag,
    uint8_t* output_val);
int verify_proof_ffi(
    const char* lurk_program,
    const char* public_params,
    const uint8_t* proof,
    size_t proof_size,
    const uint8_t* expected_tag,
    const uint8_t* expected_output);
int eval_ffi(
    const char* lurk_program,
    const char* private_params,
    const char* public_params,
	size_t* max_steps,
    uint8_t* output_tag,
    uint8_t* output_val,
	size_t* iterations,
	bool debug);
*/
import "C"
import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

const (
	// EstimatedProofSize is the estimated size (in bytes) of the transaction
	// proofs. These vary slightly for each transaction type.
	EstimatedProofSize = 12516

	// LurkMaxFieldElement is the maximum value for a field element in lurk.
	// In practice this means lurk script variables cannot exceed this value.
	LurkMaxFieldElement = "30644e72e131a029b85045b68181585d2833e84879b9709143e1f593f0000000"

	// defaultMaxSteps is the maximum number of steps to allow the prover to use
	// before terminating the proving. This large number essentially means unlimited
	// For proofs submitted by untrusted users, this number should be a lot lower.
	defaultMaxSteps uint64 = 1000000000000
)

var once sync.Once

// Expr is a Parameters type that wraps a string expression
type Expr string

func (p Expr) ToExpr() (string, error) {
	return string(p), nil
}

// List builds a list expression from a slice
func List(args ...any) Expr {
	s := ""
	for _, arg := range args {
		switch v := arg.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			s += fmt.Sprintf("(cons 0x%d ", v)
		case string:
			s += fmt.Sprintf("(cons \"%s\" ", v)
		case []byte:
			s += fmt.Sprintf("(cons 0x%x ", v)
		case bool:
			if v {
				s += "(cons t "
			} else {
				s += "(cons nil "
			}
		default:
			s += fmt.Sprintf("(cons %v ", v)
		}
	}
	if len(s) > 0 {
		s += "nil"
	}
	for i := 0; i < len(args); i++ {
		s += ")"
	}
	return Expr(s)
}

// LoadZKPublicParameters loads the lurk public parameters from disk
// into memory or generates them if this is the first startup.
func LoadZKPublicParameters() {
	once.Do(func() {
		// Redirect stderr to /dev/null
		stderrCopy := C.redirect_stderr()
		C.load_public_params()
		// Restore stderr
		C.restore_stderr(stderrCopy)
	})
}

func Prove(lurkProgram string, privateParams Parameters, publicParams Parameters, maxSteps ...uint64) ([]byte, error) {
	priv, err := privateParams.ToExpr()
	if err != nil {
		return nil, err
	}
	pub, err := publicParams.ToExpr()
	if err != nil {
		return nil, err
	}

	ms := defaultMaxSteps
	if len(maxSteps) > 0 {
		ms = maxSteps[0]
	}

	proof, tag, output, err := createProof(lurkProgram, priv, pub, ms)
	if err != nil {
		return nil, err
	}
	if tag != TagSym || !bytes.Equal(output, OutputTrue) {
		return nil, errors.New("program output is not true")
	}

	return proof, nil
}

func Verify(lurkProgram string, publicParams Parameters, proof []byte) (bool, error) {
	pub, err := publicParams.ToExpr()
	if err != nil {
		return false, err
	}
	tagBytes := make([]byte, 32)
	tagBytes[len(tagBytes)-1] = byte(TagSym)
	return verifyProof(lurkProgram, pub, proof, tagBytes, OutputTrue)
}

func Eval(lurkProgram string, privateParams Parameters, publicParams Parameters, debug ...bool) (Tag, []byte, int, error) {
	priv, err := privateParams.ToExpr()
	if err != nil {
		return TagNil, nil, 0, err
	}
	pub, err := publicParams.ToExpr()
	if err != nil {
		return TagNil, nil, 0, err
	}
	return evaluate(lurkProgram, priv, pub, defaultMaxSteps, len(debug) > 0 && debug[0])
}

func createProof(lurkProgram, privateParams, publicParams string, maxSteps uint64) ([]byte, Tag, []byte, error) {
	clurkProgram := C.CString(lurkProgram)
	cprivateParams := C.CString(privateParams)
	cpublicParams := C.CString(publicParams)

	defer C.free(unsafe.Pointer(clurkProgram))
	defer C.free(unsafe.Pointer(cprivateParams))
	defer C.free(unsafe.Pointer(cpublicParams))

	// Fixme: the actual size of the proof fluctuates
	// some. We just need to make sure this array
	// is big enough to hold it. We copy it to a
	// correctly sized slice later and then this
	// array will be freed from memory.
	// Is 15000 big enough for all proofs?
	var (
		proof     [15000]byte
		proofLen  C.size_t
		outputTag [32]byte
		outputVal [32]byte
	)

	result := C.create_proof_ffi(
		clurkProgram,
		cprivateParams,
		cpublicParams,
		(*C.size_t)(unsafe.Pointer(&maxSteps)),
		(*C.uint8_t)(unsafe.Pointer(&proof[0])),
		&proofLen,
		(*C.uint8_t)(unsafe.Pointer(&outputTag[0])),
		(*C.uint8_t)(unsafe.Pointer(&outputVal[0])),
	)

	if result != 0 {
		return nil, TagNil, nil, errors.New("failed to create proof")
	}

	var (
		proofOut = make([]byte, proofLen)
		tagOut   = make([]byte, 32)
		valOut   = make([]byte, 32)
	)
	copy(proofOut, proof[:proofLen])
	copy(tagOut, outputTag[:32])
	copy(valOut, outputVal[:32])

	tag, err := TagFromBytes(tagOut)
	if err != nil {
		return nil, TagNil, nil, err
	}

	return proofOut, tag, valOut, nil
}

func verifyProof(lurkProgram, publicParams string, proof, expectedTag, expectedOutput []byte) (bool, error) {
	if len(proof) == 0 {
		return false, errors.New("proof is nil")
	}
	clurkProgram := C.CString(lurkProgram)
	cpublicParams := C.CString(publicParams)

	defer C.free(unsafe.Pointer(clurkProgram))
	defer C.free(unsafe.Pointer(cpublicParams))

	proofCopy := make([]byte, len(proof))
	copy(proofCopy[:], proof[:])

	tagCopy := make([]byte, 32)
	copy(tagCopy, expectedTag)

	outputCopy := make([]byte, 32)
	copy(outputCopy, expectedOutput)

	// Convert the Go byte slice to a C byte pointer
	cBytesProof := (*C.uint8_t)(unsafe.Pointer(&proofCopy[0]))
	proofSize := C.size_t(len(proofCopy))
	cBytesTag := (*C.uint8_t)(unsafe.Pointer(&tagCopy[0]))
	cBytesOutput := (*C.uint8_t)(unsafe.Pointer(&outputCopy[0]))

	result := C.verify_proof_ffi(
		clurkProgram,
		cpublicParams,
		cBytesProof,
		proofSize,
		cBytesTag,
		cBytesOutput,
	)

	if result < 0 {
		return false, errors.New("proof verification errored")
	}
	return result == 0, nil
}

func evaluate(lurkProgram, privateParams, publicParams string, maxSteps uint64, debug bool) (Tag, []byte, int, error) {
	clurkProgram := C.CString(lurkProgram)
	cprivateParams := C.CString(privateParams)
	cpublicParams := C.CString(publicParams)

	defer C.free(unsafe.Pointer(clurkProgram))
	defer C.free(unsafe.Pointer(cprivateParams))
	defer C.free(unsafe.Pointer(cpublicParams))

	var (
		iterations C.size_t
		outputTag  [32]byte
		outputVal  [32]byte
	)

	result := C.eval_ffi(
		clurkProgram,
		cprivateParams,
		cpublicParams,
		(*C.size_t)(unsafe.Pointer(&maxSteps)),
		(*C.uint8_t)(unsafe.Pointer(&outputTag[0])),
		(*C.uint8_t)(unsafe.Pointer(&outputVal[0])),
		&iterations,
		C.bool(debug),
	)

	if result != 0 {
		return TagNil, nil, 0, errors.New("failed to create proof")
	}

	var (
		tagOut = make([]byte, 32)
		valOut = make([]byte, 32)
	)
	copy(tagOut, outputTag[:32])
	copy(valOut, outputVal[:32])
	iter_out := iterations

	tag, err := TagFromBytes(tagOut)
	if err != nil {
		return TagNil, nil, 0, err
	}

	return tag, valOut, int(iter_out), nil
}
