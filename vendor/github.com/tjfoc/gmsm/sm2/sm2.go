/*
Copyright Suzhou Tongji Fintech Research Institute 2017 All Rights Reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sm2

// reference to ecdsa
import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"io"
	"math/big"

	"github.com/tjfoc/gmsm/sm3"
)

var (
	default_uid = []byte{0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38}
)

const (
	aesIV = "IV for <SM2> CTR"
)

type PublicKey struct {
	elliptic.Curve
	X, Y *big.Int
}

type PrivateKey struct {
	PublicKey
	D *big.Int
}

type sm2Signature struct {
	R, S *big.Int
}

// The SM2's private key contains the public key
func (priv *PrivateKey) Public() crypto.PublicKey {
	return &priv.PublicKey
}

func SignDigitToSignData(r, s *big.Int) ([]byte, error) {
	return asn1.Marshal(sm2Signature{r, s})
}

func SignDataToSignDigit(sign []byte) (*big.Int, *big.Int, error) {
	var sm2Sign sm2Signature

	_, err := asn1.Unmarshal(sign, &sm2Sign)
	if err != nil {
		return nil, nil, err
	}
	return sm2Sign.R, sm2Sign.S, nil
}

// sign format = 30 + len(z) + 02 + len(r) + r + 02 + len(s) + s, z being what follows its size, ie 02+len(r)+r+02+len(s)+s
func (priv *PrivateKey) Sign(rand io.Reader, msg []byte, opts crypto.SignerOpts) ([]byte, error) {
	// r, s, err := Sign(priv, msg)
	r, s, err := Sm2Sign(priv, msg, default_uid)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(sm2Signature{r, s})
}

func (priv *PrivateKey) Decrypt(data []byte) ([]byte, error) {
	return Decrypt(priv, data)
}

func (pub *PublicKey) Verify(msg []byte, sign []byte) bool {
	var sm2Sign sm2Signature

	_, err := asn1.Unmarshal(sign, &sm2Sign)
	if err != nil {
		return false
	}
	return Sm2Verify(pub, msg, default_uid, sm2Sign.R, sm2Sign.S)
	// return Verify(pub, msg, sm2Sign.R, sm2Sign.S)
}

func (pub *PublicKey) Encrypt(data []byte) ([]byte, error) {
	return Encrypt(pub, data)
}

var one = new(big.Int).SetInt64(1)

func intToBytes(x int) []byte {
	var buf = make([]byte, 4)

	binary.BigEndian.PutUint32(buf, uint32(x))
	return buf
}

func kdf(length int, x ...[]byte) ([]byte, bool) {
	var c []byte

	ct := 1
	h := sm3.New()
	for i, j := 0, (length+31)/32; i < j; i++ {
		h.Reset()
		for _, xx := range x {
			h.Write(xx)
		}
		h.Write(intToBytes(ct))
		hash := h.Sum(nil)
		if i+1 == j && length%32 != 0 {
			c = append(c, hash[:length%32]...)
		} else {
			c = append(c, hash...)
		}
		ct++
	}
	for i := 0; i < length; i++ {
		if c[i] != 0 {
			return c, true
		}
	}
	return c, false
}

func randFieldElement(c elliptic.Curve, rand io.Reader) (k *big.Int, err error) {
	params := c.Params()
	b := make([]byte, params.BitSize/8+8)
	_, err = io.ReadFull(rand, b)
	if err != nil {
		return
	}
	k = new(big.Int).SetBytes(b)
	n := new(big.Int).Sub(params.N, one)
	k.Mod(k, n)
	k.Add(k, one)
	return
}

func GenerateKey() (*PrivateKey, error) {
	c := P256Sm2()
	k, err := randFieldElement(c, rand.Reader)
	if err != nil {
		return nil, err
	}
	priv := new(PrivateKey)
	priv.PublicKey.Curve = c
	priv.D = k
	priv.PublicKey.X, priv.PublicKey.Y = c.ScalarBaseMult(k.Bytes())
	return priv, nil
}

var errZeroParam = errors.New("zero parameter")

func Sign(priv *PrivateKey, hash []byte) (r, s *big.Int, err error) {
	entropylen := (priv.Curve.Params().BitSize + 7) / 16
	if entropylen > 32 {
		entropylen = 32
	}
	entropy := make([]byte, entropylen)
	_, err = io.ReadFull(rand.Reader, entropy)
	if err != nil {
		return
	}

	// Initialize an SHA-512 hash context; digest ...
	md := sha512.New()
	md.Write(priv.D.Bytes()) // the private key,
	md.Write(entropy)        // the entropy,
	md.Write(hash)           // and the input hash;
	key := md.Sum(nil)[:32]  // and compute ChopMD-256(SHA-512),
	// which is an indifferentiable MAC.

	// Create an AES-CTR instance to use as a CSPRNG.
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}

	// Create a CSPRNG that xors a stream of zeros with
	// the output of the AES-CTR instance.
	csprng := cipher.StreamReader{
		R: zeroReader,
		S: cipher.NewCTR(block, []byte(aesIV)),
	}

	// See [NSA] 3.4.1
	c := priv.PublicKey.Curve
	N := c.Params().N
	if N.Sign() == 0 {
		return nil, nil, errZeroParam
	}
	var k *big.Int
	e := new(big.Int).SetBytes(hash)
	for { // 调整算法细节以实现SM2
		for {
			k, err = randFieldElement(c, csprng)
			if err != nil {
				r = nil
				return
			}
			r, _ = priv.Curve.ScalarBaseMult(k.Bytes())
			r.Add(r, e)
			r.Mod(r, N)
			if r.Sign() != 0 {
				if t := new(big.Int).Add(r, k); t.Cmp(N) != 0 {
					break
				}
			}
		}
		rD := new(big.Int).Mul(priv.D, r)
		s = new(big.Int).Sub(k, rD)
		d1 := new(big.Int).Add(priv.D, one)
		d1Inv := new(big.Int).ModInverse(d1, N)
		s.Mul(s, d1Inv)
		s.Mod(s, N)
		if s.Sign() != 0 {
			break
		}
	}
	return
}

func Verify(pub *PublicKey, hash []byte, r, s *big.Int) bool {
	c := pub.Curve
	N := c.Params().N

	if r.Sign() <= 0 || s.Sign() <= 0 {
		return false
	}
	if r.Cmp(N) >= 0 || s.Cmp(N) >= 0 {
		return false
	}

	// 调整算法细节以实现SM2
	t := new(big.Int).Add(r, s)
	t.Mod(t, N)
	if t.Sign() == 0 {
		return false
	}

	var x *big.Int
	x1, y1 := c.ScalarBaseMult(s.Bytes())
	x2, y2 := c.ScalarMult(pub.X, pub.Y, t.Bytes())
	x, _ = c.Add(x1, y1, x2, y2)

	e := new(big.Int).SetBytes(hash)
	x.Add(x, e)
	x.Mod(x, N)
	return x.Cmp(r) == 0
}

func Sm2Sign(priv *PrivateKey, msg, uid []byte) (r, s *big.Int, err error) {
	if len(uid) == 0 {
		uid=default_uid
	}
	za, err := ZA(&priv.PublicKey, uid)
	if err != nil {
		return nil, nil, err
	}
	e, err := msgHash(za, msg)
	if err != nil {
		return nil, nil, err
	}
	c := priv.PublicKey.Curve
	N := c.Params().N
	if N.Sign() == 0 {
		return nil, nil, errZeroParam
	}
	var k *big.Int
	for { // 调整算法细节以实现SM2
		for {
			k, err = randFieldElement(c, rand.Reader)
			if err != nil {
				r = nil
				return
			}
			r, _ = priv.Curve.ScalarBaseMult(k.Bytes())
			r.Add(r, e)
			r.Mod(r, N)
			if r.Sign() != 0 {
				if t := new(big.Int).Add(r, k); t.Cmp(N) != 0 {
					break
				}
			}

		}
		rD := new(big.Int).Mul(priv.D, r)
		s = new(big.Int).Sub(k, rD)
		d1 := new(big.Int).Add(priv.D, one)
		d1Inv := new(big.Int).ModInverse(d1, N)
		s.Mul(s, d1Inv)
		s.Mod(s, N)
		if s.Sign() != 0 {
			break
		}
	}
	return
}

func Sm2Verify(pub *PublicKey, msg, uid []byte, r, s *big.Int) bool {
	c := pub.Curve
	N := c.Params().N
	one := new(big.Int).SetInt64(1)
	if r.Cmp(one) < 0 || s.Cmp(one) < 0 {
		return false
	}
	if r.Cmp(N) >= 0 || s.Cmp(N) >= 0 {
		return false
	}
	if len(uid) == 0 {
		uid=default_uid
	}
	za, err := ZA(pub, uid)
	if err != nil {
		return false
	}
	e, err := msgHash(za, msg)
	if err != nil {
		return false
	}
	t := new(big.Int).Add(r, s)
	t.Mod(t, N)
	if t.Sign() == 0 {
		return false
	}
	var x *big.Int
	x1, y1 := c.ScalarBaseMult(s.Bytes())
	x2, y2 := c.ScalarMult(pub.X, pub.Y, t.Bytes())
	x, _ = c.Add(x1, y1, x2, y2)

	x.Add(x, e)
	x.Mod(x, N)
	return x.Cmp(r) == 0
}

func msgHash(za, msg []byte) (*big.Int, error) {
	e := sm3.New()
	e.Write(za)
	e.Write(msg)
	return new(big.Int).SetBytes(e.Sum(nil)[:32]), nil
}

// ZA = H256(ENTLA || IDA || a || b || xG || yG || xA || yA)
func ZA(pub *PublicKey, uid []byte) ([]byte, error) {
	za := sm3.New()
	uidLen := len(uid)
	if uidLen >= 8192 {
		return []byte{}, errors.New("SM2: uid too large")
	}
	Entla := uint16(8 * uidLen)
	za.Write([]byte{byte((Entla >> 8) & 0xFF)})
	za.Write([]byte{byte(Entla & 0xFF)})
	if uidLen > 0 {
		za.Write(uid)
	}
	za.Write(sm2P256ToBig(&sm2P256.a).Bytes())
	za.Write(sm2P256.B.Bytes())
	za.Write(sm2P256.Gx.Bytes())
	za.Write(sm2P256.Gy.Bytes())

	xBuf := pub.X.Bytes()
	yBuf := pub.Y.Bytes()
	if n := len(xBuf); n < 32 {
		xBuf = append(zeroByteSlice()[:32-n], xBuf...)
	}
	za.Write(xBuf)
	za.Write(yBuf)
	return za.Sum(nil)[:32], nil
}

// 32byte
func zeroByteSlice() []byte {
	return []byte{
		0, 0, 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
	}
}

/*
 * sm2密文结构如下:
 *  x
 *  y
 *  hash
 *  CipherText
 */
func Encrypt(pub *PublicKey, data []byte) ([]byte, error) {
	length := len(data)
	for {
		c := []byte{}
		curve := pub.Curve
		k, err := randFieldElement(curve, rand.Reader)
		if err != nil {
			return nil, err
		}
		x1, y1 := curve.ScalarBaseMult(k.Bytes())
		x2, y2 := curve.ScalarMult(pub.X, pub.Y, k.Bytes())
		x1Buf := x1.Bytes()
		y1Buf := y1.Bytes()
		x2Buf := x2.Bytes()
		y2Buf := y2.Bytes()
		if n := len(x1Buf); n < 32 {
			x1Buf = append(zeroByteSlice()[:32-n], x1Buf...)
		}
		if n := len(y1Buf); n < 32 {
			y1Buf = append(zeroByteSlice()[:32-n], y1Buf...)
		}
		if n := len(x2Buf); n < 32 {
			x2Buf = append(zeroByteSlice()[:32-n], x2Buf...)
		}
		if n := len(y2Buf); n < 32 {
			y2Buf = append(zeroByteSlice()[:32-n], y2Buf...)
		}
		c = append(c, x1Buf...) // x分量
		c = append(c, y1Buf...) // y分量
		tm := []byte{}
		tm = append(tm, x2Buf...)
		tm = append(tm, data...)
		tm = append(tm, y2Buf...)
		h := sm3.Sm3Sum(tm)
		c = append(c, h...)
		ct, ok := kdf(length, x2Buf, y2Buf) // 密文
		if !ok {
			continue
		}
		c = append(c, ct...)
		for i := 0; i < length; i++ {
			c[96+i] ^= data[i]
		}
		return append([]byte{0x04}, c...), nil
	}
}

func Decrypt(priv *PrivateKey, data []byte) ([]byte, error) {
	data = data[1:]
	length := len(data) - 96
	curve := priv.Curve
	x := new(big.Int).SetBytes(data[:32])
	y := new(big.Int).SetBytes(data[32:64])
	x2, y2 := curve.ScalarMult(x, y, priv.D.Bytes())
	x2Buf := x2.Bytes()
	y2Buf := y2.Bytes()
	if n := len(x2Buf); n < 32 {
		x2Buf = append(zeroByteSlice()[:32-n], x2Buf...)
	}
	if n := len(y2Buf); n < 32 {
		y2Buf = append(zeroByteSlice()[:32-n], y2Buf...)
	}
	c, ok := kdf(length, x2Buf, y2Buf)
	if !ok {
		return nil, errors.New("Decrypt: failed to decrypt")
	}
	for i := 0; i < length; i++ {
		c[i] ^= data[i+96]
	}
	tm := []byte{}
	tm = append(tm, x2Buf...)
	tm = append(tm, c...)
	tm = append(tm, y2Buf...)
	h := sm3.Sm3Sum(tm)
	if bytes.Compare(h, data[64:96]) != 0 {
		return c, errors.New("Decrypt: failed to decrypt")
	}
	return c, nil
}

// keXHat 计算 x = 2^w + (x & (2^w-1))
// 密钥协商算法辅助函数
func keXHat(x *big.Int) (xul *big.Int) {
	buf := x.Bytes()
	for i := 0; i < len(buf)-16; i++ {
		buf[i] = 0
	}
	if len(buf) >= 16 {
		c := buf[len(buf)-16]
		buf[len(buf)-16] = c & 0x7f
	}

	r := new(big.Int).SetBytes(buf)
	_2w := new(big.Int).SetBytes([]byte{
		0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	return r.Add(r, _2w)
}

// keyExchange 为SM2密钥交换算法的第二部和第三步复用部分，协商的双方均调用此函数计算共同的字节串
//
// klen: 密钥长度
// ida, idb: 协商双方的标识，ida为密钥协商算法发起方标识，idb为响应方标识
// pri: 函数调用者的密钥
// pub: 对方的公钥
// rpri: 函数调用者生成的临时SM2密钥
// rpub: 对方发来的临时SM2公钥
// thisIsA: 如果是A调用，文档中的协商第三步，设置为true，否则设置为false
//
// 返回 k 为klen长度的字节串
func keyExchange(klen int, ida, idb []byte, pri *PrivateKey, pub *PublicKey,rpri *PrivateKey, rpub *PublicKey, thisISA bool) (k,s1,s2 []byte, err error) {
	curve := P256Sm2()
	N := curve.Params().N
	x2hat := keXHat(rpri.PublicKey.X)
	x2rb := new(big.Int).Mul(x2hat, rpri.D)
	tbt := new(big.Int).Add(pri.D, x2rb)
	tb := new(big.Int).Mod(tbt, N)
	if !curve.IsOnCurve(rpub.X, rpub.Y) {
		err = errors.New("Ra not on curve")
		return
	}
	x1hat := keXHat(rpub.X)
	ramx1, ramy1 := curve.ScalarMult(rpub.X, rpub.Y, x1hat.Bytes())
	vxt, vyt := curve.Add(pub.X, pub.Y, ramx1, ramy1)

	vx, vy := curve.ScalarMult(vxt, vyt, tb.Bytes())
	pza := pub
	if thisISA {
		pza = &pri.PublicKey
	}
	za, err := ZA(pza, ida)
	if err != nil {
		return
	}
	zero := new(big.Int)
	if vx.Cmp(zero) == 0 || vy.Cmp(zero) == 0 {
		err = errors.New("V is infinite")
	}
	pzb := pub
	if !thisISA {
		pzb = &pri.PublicKey
	}
	zb, err := ZA(pzb, idb)
	k, ok := kdf(klen, vx.Bytes(), vy.Bytes(), za, zb)
	if !ok {
		err = errors.New("kdf: zero key")
		return
	}
	h1:=BytesCombine(vx.Bytes(),za,zb,rpub.X.Bytes(),rpub.Y.Bytes(),rpri.X.Bytes(),rpri.Y.Bytes())
	if !thisISA {
		h1 =BytesCombine(vx.Bytes(),za,zb,rpri.X.Bytes(),rpri.Y.Bytes(),rpub.X.Bytes(),rpub.Y.Bytes())
	}
    hash:=sm3.Sm3Sum(h1)
	h2:=BytesCombine([]byte{0x02},vy.Bytes(),hash)
	S1:=sm3.Sm3Sum(h2)
	h3:=BytesCombine([]byte{0x03},vy.Bytes(),hash)
	S2:=sm3.Sm3Sum(h3)
	return k, S1,S2,nil
}
func BytesCombine(pBytes ...[]byte) []byte {
	len := len(pBytes)
	s := make([][]byte, len)
	for index := 0; index < len; index++ {
		s[index] = pBytes[index]
	}
	sep := []byte("")
	return bytes.Join(s, sep)
}
// KeyExchangeB 协商第二部，用户B调用， 返回共享密钥k
func KeyExchangeB(klen int, ida, idb []byte, priB *PrivateKey, pubA *PublicKey,rpri *PrivateKey, rpubA *PublicKey) (k,s1,s2[]byte, err error) {
	return keyExchange(klen, ida, idb, priB, pubA,rpri, rpubA, false)
}

// KeyExchangeA 协商第二部，用户A调用，返回共享密钥k
func KeyExchangeA(klen int, ida, idb []byte, priA *PrivateKey, pubB *PublicKey,rpri *PrivateKey, rpubB *PublicKey) (k,s1,s2[]byte, err error) {
	return keyExchange(klen, ida, idb, priA, pubB,rpri, rpubB, true)
}

type zr struct {
	io.Reader
}

func (z *zr) Read(dst []byte) (n int, err error) {
	for i := range dst {
		dst[i] = 0
	}
	return len(dst), nil
}

var zeroReader = &zr{}

func getLastBit(a *big.Int) uint {
	return a.Bit(0)
}

func Compress(a *PublicKey) []byte {
	buf := []byte{}
	yp := getLastBit(a.Y)
	buf = append(buf, a.X.Bytes()...)
	if n := len(a.X.Bytes()); n < 32 {
		buf = append(zeroByteSlice()[:(32-n)], buf...)
	}
	buf = append([]byte{byte(yp)}, buf...)
	return buf
}

func Decompress(a []byte) *PublicKey {
	var aa, xx, xx3 sm2P256FieldElement

	P256Sm2()
	x := new(big.Int).SetBytes(a[1:])
	curve := sm2P256
	sm2P256FromBig(&xx, x)
	sm2P256Square(&xx3, &xx)       // x3 = x ^ 2
	sm2P256Mul(&xx3, &xx3, &xx)    // x3 = x ^ 2 * x
	sm2P256Mul(&aa, &curve.a, &xx) // a = a * x
	sm2P256Add(&xx3, &xx3, &aa)
	sm2P256Add(&xx3, &xx3, &curve.b)

	y2 := sm2P256ToBig(&xx3)
	y := new(big.Int).ModSqrt(y2, sm2P256.P)
	if getLastBit(y) != uint(a[0]) {
		y.Sub(sm2P256.P, y)
	}
	return &PublicKey{
		Curve: P256Sm2(),
		X:     x,
		Y:     y,
	}
}
