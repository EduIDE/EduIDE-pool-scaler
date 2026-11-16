"""Architecture diagrams for the warm pool autoscaling (plans/2026-11-13-prewarm-pool-autoscaler.md).

Run: python3 diagrams.py            writes .drawio and .svg next to this file
     python3 diagrams.py --png DIR  also writes 2x PNG previews into DIR
"""

from pathlib import Path

import drawio_dsl as dsl
from drawio_dsl import *  # noqa: F403

dsl.OUT_DIR = Path(__file__).resolve().parent

# Unmarked elements exist on main today.
STATUS_LEGEND = [("«draft»", "Draft PR or unpushed branch only"),
                 ("«planned»", "Not built yet")]


def components_overview():
    """All subsystems; the pool scaler is a black box here (see components_pool_scaler)."""
    d = Diagram("components-overview")

    # Student and landing page
    student = actor(d, 50, 92, "Student")
    landing = subsystem(d, 160, 20, 310, 150, "EduIDE Landing\nPage", size=16)
    launch_page = component(d, 210, 92, 240, 60, "Launch Page", "pick an IDE, show launch errors")
    launch_ui = ball(d, 125, 122, "")
    d.text(80, 160, 90, 40, "Launch UI", align="center", size=13, bold=True)
    d.edge(launch_page, launch_ui, LINE, exit=(0, 0.5), entry=(1, 0.5))
    d.edge(student, launch_ui, SOCKET, exit=(1, frac((50, 92, 30, 56), 122)), entry=(0, 0.5))

    # Session service
    service = subsystem(d, 160, 270, 310, 150, "EduIDE Session\nService", size=16)
    launcher = component(d, 210, 340, 240, 60, "Session Launcher", "start sessions, time launches")
    launch_svc = ball(d, 330, 215, "Session Launch Service", label_w=180)
    d.edge(launcher, launch_svc, LINE, exit=(0.5, 0), entry=(0.5, 1))
    d.edge(launch_page, launch_svc, SOCKET, exit=(0.5, 1), entry=(0.5, 0))

    # Operator
    subsystem(d, 640, 20, 600, 400, "EduIDE\nOperator", size=16)
    handler = component(d, 680, 100, 220, 65, "Session Handler", "claim warm, fall back to cold")
    maint = component(d, 1000, 100, 210, 65, "Pool Maintenance", "periodic reconcile, capacity",
                      stereotype="component» «draft")
    pool = component(d, 680, 300, 220, 75, "Prewarmed Resource Pool", "keep targetIdle free, discard used")
    metrics = component(d, 1000, 300, 210, 75, "Pool Metrics", "pool state, claims, ready time",
                        stereotype="component» «draft")

    prov = ball(d, 555, 132, "")
    d.text(480, 74, 150, 40, "Session Provisioning<br/>Service", align="center", size=13, bold=True)
    d.edge(handler, prov, LINE, exit=(0, frac((680, 100, 220, 65), 132)), entry=(1, 0.5))
    d.edge(launcher, prov, SOCKET, exit=(1, 0.5), entry=(0.5, 1), points=[(555, 370)])

    pool_mgmt = ball(d, 790, 232, "")
    d.text(806, 212, 120, 40, "Pool Management<br/>Service", size=13, bold=True)
    d.edge(pool, pool_mgmt, LINE, exit=(0.5, 0), entry=(0.5, 1))
    d.edge(handler, pool_mgmt, SOCKET, exit=(0.5, 1), entry=(0.5, 0))
    d.edge(maint, pool_mgmt, SOCKET, exit=(round(50 / 210, 4), 1), entry=(0.5, 0),
           points=[(1050, 192), (790, 192)])

    recording = ball(d, 1105, 232, "")
    d.text(1121, 212, 115, 40, "Metric Recording<br/>Service", size=13, bold=True)
    d.edge(metrics, recording, LINE, exit=(0.5, 0), entry=(0.5, 1))
    d.edge(maint, recording, SOCKET, exit=(0.5, 1), entry=(0.5, 0))
    d.edge(pool, recording, SOCKET, exit=(1, 0.5), entry=(0, 0.5), points=[(950, 337), (950, 232)])

    # Pool scaler (black box) and monitoring
    scaler = subsystem(d, 640, 540, 300, 120, "EduIDE Pool\nScaler", size=16, stereotype="subsystem» «draft")
    d.text(656, 610, 200, 40, f'<span style="color:{MUTED};">decides free pods per app definition</span>', size=12)
    sizing = ball(d, 870, 480, "Pool Sizing Service «draft»", label_w=170)
    d.edge(pool, sizing, LINE, exit=(round(190 / 220, 4), 1), entry=(0.5, 0))
    d.edge(scaler, sizing, SOCKET, exit=(round(230 / 300, 4), 0), entry=(0.5, 1))

    mon = component(d, 1040, 540, 200, 120, "AET Cluster Monitoring", "metrics, dashboards, alerts",
                    stereotype="subsystem", external=True)
    pool_metrics = ball(d, 1105, 480, "Pool Metrics Service", label_w=140)
    d.edge(metrics, pool_metrics, LINE, exit=(round(105 / 210, 4), 1), entry=(0.5, 0))
    d.edge(mon, pool_metrics, SOCKET, exit=(round(65 / 200, 4), 0), entry=(0.5, 1))

    query = ball(d, 990, 570, "")
    d.edge(mon, query, LINE, exit=(0, frac((1040, 540, 200, 120), 570)), entry=(1, 0.5))
    d.edge(scaler, query, SOCKET, exit=(1, frac((640, 540, 300, 120), 570)), entry=(0, 0.5))
    scaler_metrics = ball(d, 990, 630, "")
    d.edge(scaler, scaler_metrics, LINE, exit=(1, frac((640, 540, 300, 120), 630)), entry=(0, 0.5))
    d.edge(mon, scaler_metrics, SOCKET, exit=(0, frac((1040, 540, 200, 120), 630)), entry=(1, 0.5))

    d.text(920, 508, 140, 40, "Metrics Query<br/>Service", align="center", size=13, bold=True)
    d.text(920, 650, 140, 40, "Scaler Metrics<br/>Service", align="center", size=13, bold=True)

    # External systems the scaler reads from
    k8s = component(d, 420, 770, 200, 70, "Kubernetes Control Plane", stereotype="subsystem", external=True)
    artemis = component(d, 690, 770, 200, 70, "Artemis", stereotype="subsystem", external=True)
    model = component(d, 945, 770, 235, 70, "Decision Model", stereotype="subsystem» «planned", external=True)
    state = ball(d, 520, 725, "")
    d.text(380, 690, 130, 40, "Cluster State<br/>Service", align="right", size=13, bold=True)
    d.edge(k8s, state, LINE, exit=(0.5, 0), entry=(0.5, 1))
    d.edge(scaler, state, SOCKET, exit=(round(40 / 300, 4), 1), entry=(1, 0.5), points=[(680, 725)])
    schedule = ball(d, 790, 725, "")
    d.text(806, 704, 140, 56, "Online IDE<br/>Schedule Service<br/>«draft»", size=13, bold=True)
    d.edge(artemis, schedule, LINE, exit=(0.5, 0), entry=(0.5, 1))
    d.edge(scaler, schedule, SOCKET, exit=(0.5, 1), entry=(0.5, 0))
    decision = ball(d, 1062, 725, "")
    d.text(1078, 712, 120, 40, "Decision<br/>Service", size=13, bold=True)
    d.edge(model, decision, LINE, exit=(0.5, 0), entry=(0.5, 1))
    d.edge(scaler, decision, SOCKET, exit=(round(270 / 300, 4), 1), entry=(0, 0.5), points=[(910, 725)])

    legend(d, 120, 450, 300, [("subsystem", "Own subsystem"), ("component", "Component"),
                              ("external", "External system"), ("interface", "Required / provided service"),
                              ("responsibility", "Responsibility")] + STATUS_LEGEND)
    d.save()


def components_pool_scaler():
    """Inside the pool scaler, by Go package. The whole subsystem is a Draft PR (EduIDE-pool-scaler#1)."""
    d = Diagram("components-pool-scaler")
    operator = subsystem(d, 480, 20, 220, 90, "EduIDE Operator", size=15)
    sizing = ball(d, 590, 155, "Pool Sizing Service «draft»", label_w=190)
    d.edge(operator, sizing, LINE, exit=(0.5, 1), entry=(0.5, 0))

    subsystem(d, 300, 200, 700, 500, "EduIDE Pool\nScaler", size=16, stereotype="subsystem» «draft")
    adapter = component(d, 480, 300, 220, 75, "Kubernetes Adapter", "read pool, quota, nodes; patch targetIdle")
    guard = component(d, 760, 300, 220, 75, "Guardrails and Budget", "floor, ceiling, rate limit, budget")
    rnd = component(d, 460, 440, 260, 80, "Scaling Round", "fallback chain, overrides, switchback")
    strategies = component(d, 330, 600, 240, 75, "Scaling Strategies", "static, reactive, predictive, decision model")
    providers = component(d, 730, 600, 240, 75, "Signal Providers", "metrics, Artemis, calendar")
    d.edge(adapter, sizing, SOCKET, exit=(0.5, 0), entry=(0.5, 1))

    pool_state = ball(d, 590, 407, "")
    d.text(440, 390, 135, 40, "Pool State<br/>Service", align="right", size=13, bold=True)
    d.edge(adapter, pool_state, LINE, exit=(0.5, 1), entry=(0.5, 0))
    d.edge(rnd, pool_state, SOCKET, exit=(round(130 / 260, 4), 0), entry=(0.5, 1))
    limit = ball(d, 870, 407, "")
    d.text(886, 390, 110, 40, "Guardrail<br/>Service", size=13, bold=True)
    d.edge(guard, limit, LINE, exit=(0.5, 1), entry=(0.5, 0))
    d.edge(rnd, limit, SOCKET, exit=(round(230 / 260, 4), 0), entry=(0, 0.5), points=[(690, 407)])

    rec = ball(d, 450, 562, "")
    d.text(330, 520, 110, 40, "Recommendation<br/>Service", align="right", size=13, bold=True)
    d.edge(strategies, rec, LINE, exit=(0.5, 0), entry=(0.5, 1))
    d.edge(rnd, rec, SOCKET, exit=(round(70 / 260, 4), 1), entry=(1, 0.5), points=[(530, 562)])
    signal = ball(d, 850, 562, "")
    d.text(866, 520, 110, 40, "Signal<br/>Service", size=13, bold=True)
    d.edge(providers, signal, LINE, exit=(0.5, 0), entry=(0.5, 1))
    d.edge(rnd, signal, SOCKET, exit=(round(190 / 260, 4), 1), entry=(0, 0.5), points=[(650, 562)])

    k8s = component(d, 40, 300, 200, 75, "Kubernetes Control Plane", stereotype="subsystem", external=True)
    state = ball(d, 270, 337, "")
    d.text(160, 270, 130, 40, "Cluster State<br/>Service", align="right", size=13, bold=True)
    d.edge(k8s, state, LINE, exit=(1, 0.5), entry=(0, 0.5))
    d.edge(adapter, state, SOCKET, exit=(0, frac((480, 300, 220, 75), 337)), entry=(1, 0.5))

    mon = component(d, 1130, 440, 220, 230, "AET Cluster Monitoring", "metrics, dashboards, alerts",
                    stereotype="subsystem", external=True)
    scaler_metrics = ball(d, 800, 480, "")
    d.text(740, 492, 120, 40, "Scaler Metrics<br/>Service", align="center", size=13, bold=True)
    d.edge(rnd, scaler_metrics, LINE, exit=(1, frac((460, 440, 260, 80), 480)), entry=(0, 0.5))
    d.edge(mon, scaler_metrics, SOCKET, exit=(0, frac((1130, 440, 220, 230), 480)), entry=(1, 0.5))
    query = ball(d, 1065, 637, "")
    d.text(1005, 655, 120, 40, "Metrics Query<br/>Service", align="center", size=13, bold=True)
    d.edge(mon, query, LINE, exit=(0, frac((1130, 440, 220, 230), 637)), entry=(1, 0.5))
    d.edge(providers, query, SOCKET, exit=(1, frac((730, 600, 240, 75), 637)), entry=(0, 0.5))

    model = component(d, 340, 800, 220, 70, "Decision Model", stereotype="subsystem» «planned", external=True)
    decision = ball(d, 450, 750, "Decision Service", label_w=130)
    d.edge(model, decision, LINE, exit=(0.5, 0), entry=(0.5, 1))
    d.edge(strategies, decision, SOCKET, exit=(round(120 / 240, 4), 1), entry=(0.5, 0))
    artemis = component(d, 740, 800, 220, 70, "Artemis", stereotype="subsystem", external=True)
    schedule = ball(d, 850, 750, "")
    d.text(866, 722, 150, 56, "Online IDE<br/>Schedule Service<br/>«draft»", size=13, bold=True)
    d.edge(artemis, schedule, LINE, exit=(0.5, 0), entry=(0.5, 1))
    d.edge(providers, schedule, SOCKET, exit=(0.5, 1), entry=(0.5, 0))

    legend(d, 1060, 20, 300, [("subsystem", "Own subsystem"), ("component", "Component"),
                              ("external", "External system"), ("interface", "Required / provided service"),
                              ("responsibility", "Responsibility")] + STATUS_LEGEND)
    d.save()


def deployment():
    """TUM production on the AET cluster."""
    d = Diagram("deployment-tum-production")
    browser = node(d, 20, 400, 220, 150, "device", "Student Browser", "Browser · React SPA")
    subsystem(d, 35, 480, 190, 55, "EduIDE Landing Page", size=12)

    node(d, 340, 20, 1000, 760, "executionEnvironment", "AET Kubernetes Cluster", "Kubernetes · Rancher")
    api = node(d, 380, 100, 920, 140, "executionEnvironment", "kube-apiserver", "Kubernetes API · HTTPS :6443",
               fill=EXTERNAL_FILL)
    component(d, 700, 180, 260, 50, "Kubernetes Control Plane", stereotype="subsystem", external=True)

    group(d, 380, 330, 920, 230, "")
    d.text(394, 520, 300, 36, f'<span style="color:{ACCENT};"><b>namespace eduide-tum-production</b><br/>'
           f'ResourceQuota requests.cpu 200</span>', size=12)
    svc = node(d, 400, 350, 200, 160, "pod", "service-deployment", "Quarkus · Java 21")
    subsystem(d, 412, 440, 176, 55, "EduIDE Session Service", size=12)
    op = node(d, 620, 350, 220, 160, "pod", "operator-deployment ×3",
              "Java 21 · leader election<br/>/metrics :9464")
    subsystem(d, 635, 440, 190, 55, "EduIDE Operator", size=13)
    ide = node(d, 860, 350, 200, 160, "pod", "instance-n, session-n", "EduIDE IDE image<br/>nodeSelector pool=pve")
    d.text(870, 440, 180, 60, f'<span style="color:{MUTED};">warm pool instances and<br/>cold sessions, created<br/>by the operator</span>', size=11)
    sc = node(d, 1080, 350, 200, 160, "pod", "pool-scaler", "Go · 1 replica, Recreate<br/>/metrics")
    subsystem(d, 1092, 440, 176, 55, "EduIDE Pool Scaler", size=12, stereotype="subsystem» «draft")

    group(d, 600, 620, 700, 140, "")
    d.text(860, 626, 260, 24, f'<span style="color:{ACCENT};"><b>namespace cattle-monitoring-system</b></span>', size=12)
    mon = node(d, 620, 650, 660, 100, "pod", "Rancher Monitoring", "Prometheus · Grafana · Alertmanager · :9090",
               fill=EXTERNAL_FILL)
    component(d, 1000, 680, 260, 50, "AET Cluster Monitoring", stereotype="subsystem", external=True)

    note(d, 380, 640, 200, 120, "Session pods run only on the 20 pve workers (15.6 CPU, 25.8 GiB each); "
         "free pool pods count against the quota.")

    art = node(d, 1480, 250, 260, 150, "executionEnvironment", "Artemis Server", "Spring Boot · HTTPS :443",
               fill=EXTERNAL_FILL)
    component(d, 1500, 330, 220, 50, "Artemis", stereotype="subsystem", external=True)
    dm = node(d, 1480, 460, 260, 150, "executionEnvironment", "Decision Model Host", "Open-Jev style model",
              fill=EXTERNAL_FILL)
    component(d, 1500, 540, 220, 50, "Decision Model", stereotype="subsystem» «planned", external=True)

    # paths, all on node borders
    d.edge(browser, svc, LINE, exit=(1, frac((20, 400, 220, 150), 430)), entry=(0, frac((400, 350, 200, 160), 430)))
    d.text(244, 360, 94, 70, "<b>Session<br/>Launch</b><br/>HTTPS :443<br/>Envoy Gateway", align="center", size=12)
    d.edge(svc, api, LINE, exit=(0.5, 0), entry=(round(120 / 920, 4), 1))
    d.text(506, 254, 110, 60, "<b>Session<br/>Provisioning</b><br/>HTTPS", size=12)
    d.edge(op, api, LINE, exit=(0.5, 0), entry=(round(350 / 920, 4), 1))
    d.text(736, 254, 120, 60, "<b>Session Provis.,<br/>Pool Sizing</b><br/>watch · HTTPS", size=12)
    d.edge(sc, api, LINE, exit=(0.5, 0), entry=(round(800 / 920, 4), 1))
    d.text(1186, 254, 120, 60, "<b>Pool Sizing,<br/>Cluster State</b><br/>HTTPS", size=12)
    d.edge(mon, op, LINE, exit=(round(110 / 660, 4), 0), entry=(0.5, 1))
    d.text(736, 566, 140, 34, "<b>Pool Metrics</b><br/>HTTP :9464", size=12)
    d.edge(mon, sc, LINE, exit=(round(560 / 660, 4), 0), entry=(0.5, 1))
    d.text(1190, 566, 145, 34, "<b>Metrics Query,<br/>Scaler Metrics</b> · HTTP", size=12)
    d.edge(sc, art, LINE, exit=(1, frac((1080, 350, 200, 160), 380)), entry=(0, frac((1480, 250, 260, 150), 380)))
    d.text(1348, 304, 128, 70, "<b>Online IDE<br/>Schedule</b> «draft»<br/>HTTPS :443", align="center", size=12)
    d.edge(sc, dm, LINE, exit=(1, frac((1080, 350, 200, 160), 480)), entry=(0, frac((1480, 460, 260, 150), 480)))
    d.text(1348, 488, 128, 40, "<b>Decision</b><br/>HTTPS · token", align="center", size=12)

    legend(d, 20, 600, 280, [("subsystem", "Own subsystem"), ("external", "External system"),
                             ("namespace", "Kubernetes namespace"), ("technology", "Technology, version, port"),
                             ("note", "Note")] + STATUS_LEGEND)
    d.save()


if __name__ == "__main__":
    components_overview()
    components_pool_scaler()
    deployment()
