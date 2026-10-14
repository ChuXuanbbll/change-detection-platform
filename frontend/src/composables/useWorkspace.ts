import { computed, inject, provide, ref, watch, onUnmounted, type InjectionKey } from 'vue';
import { settingsState, refreshSettings } from '../utils/settings';
import { useDetection, type ResultOverlayPayload } from './useDetection';
import type { AreaBounds, AreaPolygon } from '../utils/api';
import { getRegionData, type RegionFeature, type RegionScenes, type ImageryPhase } from '../utils/regions';

function createWorkspace() {
  const detection = useDetection();
  const selectedArea = ref<AreaBounds | null>(null);
  const selectedGeometry = ref<AreaPolygon | null>(null);
  const selectionEditing = ref(false);
  function setSelectedArea(bounds: AreaBounds | null, geometry: AreaPolygon | null = null) {
    if (detection.inProgress.value) return;
    detection.resetDetection();
    selectedArea.value = bounds;
    selectedGeometry.value = bounds && geometry ? JSON.parse(JSON.stringify(geometry)) : null;
  }
  const selectedRegion = ref<RegionFeature | null>(null);
  const catalogPhases = ref<ImageryPhase[]>([]);
  const loadedPhaseIds = ref<string[]>([]);
  const imageryPhases = computed(() => catalogPhases.value.filter(phase => phase.available && loadedPhaseIds.value.includes(phase.id)));
  const loadedScenes = computed(() => imageryPhases.value.flatMap(phase => phase.scenes.filter(scene => scene.available)));
  const activeMonth = ref('');
  const catalogLoading = ref(false);
  const catalogError = ref('');
  let catalogRequest: AbortController | null = null;
  const indexLoading = ref(false);
  const indexError = ref('');
  let indexRequest: AbortController | null = null;
  let indexReady: Promise<void> = Promise.resolve();
  function initializeCatalog(): Promise<void> {
    if (indexLoading.value) return indexReady;
    indexLoading.value = true; indexError.value = '';
    const request = new AbortController(); indexRequest = request;
    indexReady = getRegionData<unknown>('/py/local/catalog?refresh=true', request.signal)
      .then(() => {})
      .catch(error => {
        if (!request.signal.aborted) indexError.value = error instanceof Error ? error.message : '影像目录初始化失败';
      })
      .finally(() => { indexLoading.value = false; });
    return indexReady;
  }
  async function retryIndex() {
    await initializeCatalog();
    if (!indexError.value) await refreshCatalog(false);
  }
  // App provides one workspace per document; SPA navigation reuses this promise.
  void initializeCatalog();
  onUnmounted(() => { catalogRequest?.abort(); indexRequest?.abort(); });
  const activePhase = computed(() => imageryPhases.value.find(phase => phase.month === activeMonth.value));
  async function selectWorkspaceRegion(region: RegionFeature | null) {
    if (detection.inProgress.value || selectedRegion.value?.id === region?.id) return;
    catalogRequest?.abort();
    selectedRegion.value = region;
    setSelectedArea(null);
    loadedPhaseIds.value = []; activeMonth.value = ''; catalogPhases.value = [];
    catalogError.value = ''; catalogLoading.value = false;
    detection.resetDetection();
    if (!region) return;
    await refreshCatalog(false);
  }
  async function refreshCatalog(force = true) {
    const region = selectedRegion.value;
    if (!region || detection.inProgress.value) return;
    catalogRequest?.abort();
    const request = new AbortController(); catalogRequest = request;
    catalogLoading.value = true;
    catalogError.value = '';
    try {
      await indexReady;
      if (request.signal.aborted) return;
      if (indexError.value && !force) throw new Error(indexError.value);
      const data = await getRegionData<RegionScenes>(`/py/local/regions/${encodeURIComponent(region.id)}/scenes${force ? '?refresh=true' : ''}`, request.signal);
      if (!request.signal.aborted && selectedRegion.value?.id === region.id) {
        indexError.value = '';
        const previous = imageryPhases.value.map(phase => `${phase.id}:${phase.version}`).join('|');
        catalogPhases.value = data.phases;
        loadedPhaseIds.value = imageryPhases.value.map(phase => phase.id);
        if (imageryPhases.value.map(phase => `${phase.id}:${phase.version}`).join('|') !== previous) detection.resetDetection();
        if (!imageryPhases.value.some(phase => phase.month === activeMonth.value)) activeMonth.value = imageryPhases.value[0]?.month || '';
      }
    } catch (error) {
      if (!request.signal.aborted) catalogError.value = error instanceof Error ? error.message : '影像列表加载失败';
    } finally { if (catalogRequest === request) catalogLoading.value = false; }
  }
  function addPhase(phaseId: string) {
    if (detection.inProgress.value) return;
    const phase = catalogPhases.value.find(item => item.id === phaseId);
    if (!phase?.available || phase.region_id !== selectedRegion.value?.id) return;
    if (!loadedPhaseIds.value.includes(phaseId)) {
      loadedPhaseIds.value = [...loadedPhaseIds.value, phaseId];
      detection.resetDetection();
    }
    activeMonth.value = phase.month;
  }
  function removePhase(phaseId: string) {
    if (detection.inProgress.value) return;
    loadedPhaseIds.value = loadedPhaseIds.value.filter(id => id !== phaseId);
    detection.resetDetection();
    if (!imageryPhases.value.some(phase => phase.month === activeMonth.value)) activeMonth.value = imageryPhases.value[0]?.month || '';
  }
  function closePhase(month: string) {
    if (detection.inProgress.value) return;
    const phase = imageryPhases.value.find(item => item.month === month);
    if (phase) removePhase(phase.id);
  }
  const selectedPairKey = ref('');
  const results = ref<ResultOverlayPayload[]>([]);
  const selectedPair = computed(() => detection.pairs.value.find(pair => pair.key === selectedPairKey.value));
  detection.onResult(result => { results.value.push(result); });
  watch(() => detection.pairs.value, () => {
    results.value = [];
    selectedPairKey.value = '';
  }, { flush: 'sync' });
  watch(() => detection.pairs.value.map(pair => `${pair.key}:${pair.status}`).join('|'), () => {
    if (!selectedPairKey.value) {
      selectedPairKey.value = detection.pairs.value.find(pair => pair.status === 'succeeded')?.key || '';
    }
  });
  let observedSources = "";
  let pendingRevision = false;
  watch(() => JSON.stringify(settingsState.value?.settings.sources), sources => {
    if (!sources) return;
    if (observedSources && observedSources !== sources) pendingRevision = true;
    observedSources = sources;
    if (pendingRevision && !detection.inProgress.value) void updateConfiguration();
  });
  watch(detection.inProgress, running => { if (!running && pendingRevision) void updateConfiguration(); });
  async function updateConfiguration() {
    pendingRevision = false;
    setSelectedArea(null); loadedPhaseIds.value = []; activeMonth.value = '';
    await retryIndex();
  }
  const refreshOnFocus = () => { void refreshSettings().catch(() => {}); };
  const warnOnClose = (event: BeforeUnloadEvent) => { if (detection.inProgress.value) { event.preventDefault(); event.returnValue = ''; } };
  window.addEventListener('focus', refreshOnFocus);
  window.addEventListener('beforeunload', warnOnClose);
  onUnmounted(() => { window.removeEventListener('focus', refreshOnFocus); window.removeEventListener('beforeunload', warnOnClose); });
  return { ...detection, selectedArea, selectedGeometry, selectionEditing, setSelectedArea, selectedPairKey, selectedPair, results,
    selectedRegion, catalogPhases, loadedPhaseIds, loadedScenes, activeMonth, imageryPhases, activePhase,
    indexLoading, indexError, retryIndex,
    catalogLoading, catalogError, selectWorkspaceRegion, refreshCatalog, addPhase, removePhase, closePhase };
}

type Workspace = ReturnType<typeof createWorkspace>;
const workspaceKey: InjectionKey<Workspace> = Symbol('detection-workspace');
export function provideWorkspace() {
  const workspace = createWorkspace();
  provide(workspaceKey, workspace);
  return workspace;
}
export function useWorkspace() {
  const workspace = inject(workspaceKey);
  if (!workspace) throw new Error('Detection workspace is unavailable');
  return workspace;
}
