package wghelper

import (
	"context"
	"fmt"
	"net"
	"time"

	registrar "github.com/jaredallard-home/worker-nodes/registrar/apis/clientset/v1alpha1"
	"github.com/jaredallard-home/worker-nodes/registrar/apis/types/v1alpha1"
	"github.com/pkg/errors"
	wgnetlink "github.com/schu/wireguard-cni/pkg/netlink"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Wireguard struct {
	device *wgtypes.Device
	w      *wgctrl.Client
	k      *registrar.RegistrarClientset
	l      netlink.Link
}

// NewWirguard creates a new wireguard configuration instance, that stores
// IP information in Kubernetes
func NewWireguard(k *registrar.RegistrarClientset) (*Wireguard, error) {
	w, err := wgctrl.New()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create wireguard controller")
	}

	resp := &Wireguard{
		w: w,
		k: k,
	}

	devices, err := w.Devices()
	if err != nil {
		return nil, errors.Wrap(err, "failed to list wireguard devices")
	}

	if len(devices) > 1 {
		return nil, fmt.Errorf("found more than one wireguard device, only one is supported")
	}

	// attempt to create a wireguard interface
	if len(devices) == 0 {
		log.Infof("creating a wireguard interface")

		attrs := netlink.NewLinkAttrs()
		attrs.Name = "wg0"

		l := &wgnetlink.Wireguard{
			LinkAttrs: attrs,
		}

		if err := netlink.LinkAdd(l); err != nil {
			return nil, errors.Wrap(err, "failed to create link")
		}

		resp.device, err = w.Device(attrs.Name)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get created wireguard link")
		}
	} else {
		resp.device = devices[0]
	}

	resp.l, err = netlink.LinkByName(resp.device.Name)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get link by device name")
	}

	return resp, nil
}

func (w *Wireguard) StartServer(ipool *v1alpha1.WireguardIPPool) error {
	// TODO(jaredallard): better way to do this?
	if w.device.PrivateKey.String() == "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
		log.Info("failed to find initialized device, creating new server")
		if err := w.initServer(ipool); err != nil {
			return errors.Wrap(err, "failed to init server ")
		}
	}

	ip, _, err := net.ParseCIDR(ipool.Spec.CIDR)
	if err != nil {
		return errors.Wrap(err, "failed to parse CIDR")
	}

	err = netlink.AddrReplace(w.l, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: net.IPv4bcast.DefaultMask(),
		},
	})
	if err != nil {
		return errors.Wrap(err, "failed to assign IP to wg0")
	}

	if err := netlink.LinkSetUp(w.l); err != nil {
		return errors.Wrap(err, "failed to set link to up")
	}

	log.Info("wireguard server started")

	return nil
}

// initServer initializes a new wireguard server
func (w *Wireguard) initServer(ipool *v1alpha1.WireguardIPPool) error {
	if ipool.Status.SecretRef == "" {
		log.Info("failed to find a secret key for this ippool, creating new one")
		privk, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return errors.Wrap(err, "failed to generate private key")
		}

		// TODO(jaredallard): default hardcode
		secretName := fmt.Sprintf("wgipp-%s", ipool.ObjectMeta.Name)
		_, err = w.k.CoreV1().Secrets("default").Create(context.TODO(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: secretName,
			},
			StringData: map[string]string{
				"privk": privk.String(),
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return errors.Wrap(err, "failed to store wireguard secret in kubernetes")
		}

		ipool.Status.SecretRef = secretName
		ipool.Status.Created = true

		_, err = w.k.RegistrarV1Alpha1Client().WireguardIPPools("default").Update(context.TODO(), ipool)
		if err != nil {
			return errors.Wrap(err, "failed to update ipool in k8s")
		}
	}

	sec, err := w.k.CoreV1().Secrets("default").Get(context.TODO(), ipool.Status.SecretRef, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to get wireguard server privk")
	}

	privk, err := wgtypes.ParseKey(string(sec.Data["privk"]))
	if err != nil {
		return errors.Wrap(err, "failed to parse wireguard server privk")
	}

	// add the peer to our device
	err = w.w.ConfigureDevice(w.device.Name, wgtypes.Config{
		ReplacePeers: true,
		PrivateKey:   &privk,
	})
	if err != nil {
		return errors.Wrap(err, "failed to configure wireguard device")
	}

	return nil
}

// Register adds a new peer to a device, and returns the information needed to connect
// as said peer
func (w *Wireguard) Register(ip *v1alpha1.WireguardIP) (*wgtypes.PeerConfig, error) {
	privk, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate private key")
	}

	pki := 5 * time.Second

	peer := &wgtypes.PeerConfig{
		PublicKey:         privk.PublicKey(),
		PresharedKey:      &privk,
		UpdateOnly:        false,
		ReplaceAllowedIPs: true,
		// Allows this peer to survive when running behind NAT
		PersistentKeepaliveInterval: &pki,
		AllowedIPs: []net.IPNet{
			{
				IP: net.ParseIP(ip.Spec.IPAdress),
				// Default well-known broadcast. This might have to be changed?
				Mask: net.IPv4bcast.DefaultMask(),
			},
		},
	}

	// add the peer to our device
	err = w.w.ConfigureDevice(w.device.Name, wgtypes.Config{
		Peers: []wgtypes.PeerConfig{*peer},
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to configure wireguard device")
	}

	return peer, err
}